# Requires Linux with KVM: nix build .#checks.x86_64-linux.headscale -L
{
  pkgs,
  self,
  headscale,
}:
let
  policy = acls: pkgs.writeText "folio-policy.json" (builtins.toJSON { inherit acls; });
  allow = policy [
    {
      action = "accept";
      src = [ "*" ];
      dst = [ "*:*" ];
    }
  ];
  restricted = policy [
    {
      action = "accept";
      src = [ "alice@" ];
      dst = [ "server@:*" ];
    }
    {
      action = "accept";
      src = [ "bob@" ];
      dst = [ "server@:80" ];
    }
  ];
  peer = {
    imports = [ headscale.nixosModules.testkit-peer ];
    environment.systemPackages = [
      pkgs.curl
      pkgs.jq
    ];
    virtualisation.memorySize = 512;
  };
in
pkgs.testers.runNixOSTest {
  name = "folio-headscale";
  nodes = {
    headscale = {
      imports = [ headscale.nixosModules.testkit ];
      virtualisation.memorySize = 1024;
      # Advertise certificate domains; the cached certificate avoids public ACME.
      services.headscale.settings.dns = {
        certificates.enabled = true;
        challenge.provider = {
          name = "rfc2136";
          server = "127.0.0.1:53";
          key_name = "test";
          key_alg = "hmac-sha256";
          key = "c2VjcmV0";
        };
      };
    };
    machine = { lib, ... }: {
      imports = [ self.nixosModules.default ];
      services.folio = {
        enable = true;
        hostname = "notes";
        authKeyFile = "/run/folio-authkey";
        logLevel = "debug";
      };
      systemd.services.folio = {
        wantedBy = lib.mkForce [ ];
        environment = {
          TS_CONTROL_URL = "http://headscale";
          TS_NO_LOGS_NO_SUPPORT = "1";
          SSL_CERT_FILE = "/var/lib/folio/test-ca.crt";
        };
      };
      # tsnet picks an ephemeral UDP port; permit direct traffic on the test VLAN.
      networking.firewall.allowedUDPPortRanges = [
        {
          from = 1024;
          to = 65535;
        }
      ];
      environment.systemPackages = [ pkgs.openssl ];
      virtualisation.memorySize = 2048;
    };
    alice = peer;
    bob = {
      imports = [ peer ];
      systemd.services.tailscaled.environment.TS_DEBUG_ALWAYS_USE_DERP = "1";
    };
  };
  testScript = ''
    import json
    import shlex

    state = "/var/lib/folio"

    def request(node, path, method="GET", data=None, status=200, headers=()):
        args = ["curl", "--silent", "--show-error", "--max-time", "10",
                "--cacert", "/tmp/app.crt", "-X", method, "-D", "/tmp/response.headers", "-w", "\\n%{http_code}"]
        for header in headers:
            args += ["-H", header]
        if data is not None:
            args += ["-H", "Content-Type: application/json", "--data", json.dumps(data)]
        output = node.succeed(shlex.join(args + ["https://notes.tailnet" + path]))
        body, code = output.rsplit("\n", 1)
        assert int(code) == status, (node.name, path, code, body)
        if "data: " in body:
            body = next(line[6:] for line in body.splitlines() if line.startswith("data: "))
        return json.loads(body) if body else None

    def ready(node):
        node.wait_until_succeeds(
            "curl -fsS --max-time 5 --cacert /tmp/app.crt https://notes.tailnet/api/me", timeout=120)

    def service_route():
        routes = json.loads(machine.succeed("ip -j -4 route show default"))
        assert len(routes) == 1 and routes[0]["dev"] == lan and routes[0]["gateway"] == gateway, routes
        machine.succeed("journalctl -u folio _PID=$(systemctl show -p MainPID --value folio) -o cat | "
                        "grep -F " + shlex.quote("defaultRoute=" + lan + " "))

    mcp_headers = ["Accept: application/json, text/event-stream", "MCP-Protocol-Version: 2025-03-26"]

    def mcp(method, params):
        response = request(bob, "/mcp", "POST", {
            "jsonrpc": "2.0", "id": 1, "method": method, "params": params,
        }, headers=mcp_headers)
        assert "error" not in response, response
        return response["result"]

    start_all()
    machine.wait_for_unit("multi-user.target")
    gateway = machine.succeed("getent ahostsv4 headscale | awk 'NR == 1 {print $1}'").strip()
    lan = json.loads(machine.succeed(f"ip -j route get {gateway}"))[0]["dev"]
    # Unprivileged tsnet binds to the default interface; peers live on the VLAN.
    machine.succeed(f"ip link set eth0 down; ip -4 route flush default; ip -6 route flush default; "
                    f"ip route add default via {gateway} dev {lan}")
    for node in [alice, bob]:
        key = headscale.succeed(f"hs-authkey {node.name}").strip()
        node.succeed(f"hs-join {key}")
    headscale.succeed("headscale policy set -f ${allow}")

    with subtest("upstream service enrolls and serves cached TLS"):
        machine.succeed(f"install -d -o folio -g folio -m 700 {state}/tsnet/certs")
        machine.succeed(
            f"openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=notes.tailnet "
            f"-addext subjectAltName=DNS:notes.tailnet -keyout {state}/tsnet/certs/notes.tailnet.key "
            f"-out {state}/tsnet/certs/notes.tailnet.crt")
        machine.succeed(f"cp {state}/tsnet/certs/notes.tailnet.crt {state}/test-ca.crt; "
                        f"chown -R folio:folio {state}/tsnet {state}/test-ca.crt; chmod 600 {state}/tsnet/certs/*.key")
        cert = machine.succeed(f"cat {state}/test-ca.crt")
        for node in [alice, bob]:
            node.succeed(f"printf %s {shlex.quote(cert)} > /tmp/app.crt")
        key = headscale.succeed("hs-authkey server").strip()
        machine.succeed(f"umask 077; printf %s {shlex.quote(key)} > /run/folio-authkey; systemctl start folio")
        ready(alice)
        ready(bob)
        service_route()
        assert alice.execute("curl -fsS --max-time 5 https://notes.tailnet/api/me")[0] == 60
        ready(alice)
        assert not machine.succeed("ss -H -ltn '( sport = :80 or sport = :443 )'").strip()
        a, b = request(alice, "/api/me"), request(bob, "/api/me")
        assert a["login"] == "alice" and b["login"] == "bob" and a["vault"] != b["vault"], (a, b)

    with subtest("direct and forced DERP carry application traffic"):
        alice.wait_until_succeeds("tailscale ping --timeout 3s -c 1 notes", timeout=120)
        alice.wait_until_succeeds(
            "tailscale status --json | jq -e '.Peer[] | select(.HostName == \"notes\") | (.CurAddr // \"\") != \"\"'", timeout=120)
        request(alice, "/api/vaults/me/notes", "POST", {"path": "shared.md", "content": "direct note"}, 201)
        shared_path = "/api/vaults/" + a["vault"] + "/notes/shared.md"
        assert "direct note" in request(alice, shared_path)["content"]
        bob.succeed("tailscale ping --until-direct=false --timeout 5s -c 3 notes | grep 'via DERP(headscale)'")
        request(bob, "/api/vaults/me/notes", "POST", {"path": "relay.md", "content": "DERP note"}, 201)
        assert "DERP note" in request(bob, "/api/vaults/me/notes/relay.md")["content"]
        request(bob, shared_path, status=403)

    with subtest("existing shares and stateful MCP work over DERP"):
        grant = request(alice, "/api/shares", "POST", {"path": "shared.md", "grantee": "bob", "perm": "read"}, 201)
        original = request(bob, shared_path)
        request(bob, shared_path, "PUT", {"content": "denied edit"}, 403)
        assert request(alice, shared_path)["content"] == original["content"]
        request(alice, "/api/shares", "POST", {"path": "shared.md", "grantee": "bob", "perm": "write"}, 201)
        request(bob, shared_path, "PUT", {"content": "shared edit"}, headers=["If-Match: " + original["sha256"]])
        shared_content = request(alice, shared_path)["content"]
        assert "shared edit" in shared_content
        request(alice, "/api/shares", "POST", {"path": "shared.md", "grantee": "bob", "perm": "read"}, 201)

        init = mcp("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                  "clientInfo": {"name": "vm-test", "version": "1"}})
        assert "tools" in init["capabilities"], init
        headers = bob.succeed("cat /tmp/response.headers")
        session = next(line.split(":", 1)[1].strip() for line in headers.splitlines()
                       if line.lower().startswith("mcp-session-id:"))
        assert session
        mcp_headers.append("Mcp-Session-Id: " + session)
        request(bob, "/mcp", "POST", {"jsonrpc": "2.0", "method": "notifications/initialized"},
                status=202, headers=mcp_headers)
        assert "read_note" in [tool["name"] for tool in mcp("tools/list", {})["tools"]]
        resource = mcp("resources/read", {"uri": "folio://" + a["vault"] + "/shared.md"})
        assert resource["contents"][0]["text"] == shared_content, resource
        denied = mcp("tools/call", {"name": "append_note", "arguments": {
            "vault": a["vault"], "path": "shared.md", "text": "denied append"}})
        assert denied.get("isError") and "permission denied" in json.dumps(denied), denied
        assert request(alice, shared_path)["content"] == shared_content
        allowed = mcp("tools/call", {"name": "append_note", "arguments": {"path": "relay.md", "text": "MCP append"}})
        assert not allowed.get("isError"), allowed
        assert "MCP append" in request(bob, "/api/vaults/me/notes/relay.md")["content"]
        allowed = mcp("tools/call", {"name": "read_note", "arguments": {"vault": a["vault"], "path": "shared.md"}})
        assert not allowed.get("isError") and allowed["structuredContent"]["content"] == shared_content, allowed
        request(alice, "/api/shares/" + grant["id"], "DELETE", status=204)
        request(bob, shared_path, status=403)
        denied = mcp("tools/call", {"name": "read_note", "arguments": {"vault": a["vault"], "path": "shared.md"}})
        assert denied.get("isError") and "permission denied" in json.dumps(denied), denied
        request(alice, "/api/shares", "POST", {"path": "shared.md", "grantee": "bob", "perm": "read"}, 201)
        assert request(bob, shared_path)["content"] == shared_content

    with subtest("live ACL denies and reallows DERP HTTPS with an allowed control"):
        headscale.succeed("headscale policy set -f ${restricted}")
        blocked_https = (
            "code=0; http=$(curl -sS --max-time 3 --cacert /tmp/app.crt -o /dev/null "
            "-w '%{http_code}' https://notes.tailnet" + shared_path + ") || code=$?; "
            "test \"$code:$http\" = '28:000'")
        bob.wait_until_succeeds(blocked_https, timeout=120)
        assert request(alice, shared_path)["content"] == shared_content
        bob.succeed("curl -sS --max-time 5 -D /tmp/headers http://notes.tailnet/api/me -o /dev/null; "
                    "grep '301' /tmp/headers; grep -i 'location: https://notes.tailnet/api/me' /tmp/headers")
        bob.succeed(blocked_https)
        headscale.succeed("headscale policy set -f ${allow}")
        ready(bob)
        assert request(bob, shared_path)["content"] == shared_content
        assert request(alice, shared_path)["content"] == shared_content
        bob.succeed("tailscale ping --until-direct=false --timeout 5s -c 3 notes | grep 'via DERP(headscale)'")

    with subtest("restart preserves enrollment, identities, notes and shares"):
        before = json.loads(headscale.succeed("headscale nodes list -o json"))
        machine.succeed("systemctl stop folio; : > /run/folio-authkey; systemctl start folio")
        ready(alice)
        ready(bob)
        service_route()
        after = json.loads(headscale.succeed("headscale nodes list -o json"))
        def node_id(nodes):
            return [(node["id"], node["ipAddresses"]) for node in nodes if node["givenName"] == "notes"]
        assert node_id(before) and node_id(before) == node_id(after), (before, after)
        assert request(alice, "/api/me") == a and request(bob, "/api/me") == b
        assert request(bob, shared_path)["content"] == shared_content
        request(bob, shared_path, "PUT", {"content": "denied after restart"}, 403)
        assert request(alice, shared_path)["content"] == shared_content
        assert "MCP append" in request(bob, "/api/vaults/me/notes/relay.md")["content"]
        machine.succeed(f"stat -c '%U:%G %a' {state} | grep -x 'folio:folio 700'")
        machine.succeed("test $(systemctl show -p User --value folio) = folio; "
                        "test -z \"$(systemctl show -p CapabilityBoundingSet --value folio)\"")
  '';
}
