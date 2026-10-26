# Requires Linux with KVM: nix build .#checks.x86_64-linux.nixos-vm -L
{
  pkgs,
  self,
  headscale,
}:
let
  policy =
    acls:
    pkgs.writeText "folio-policy.json" (
      builtins.toJSON {
        tagOwners = {
          "tag:agent" = [ "alice@" ];
          "tag:unmapped" = [ "alice@" ];
          "tag:missing" = [ "alice@" ];
        };
        inherit acls;
      }
    );
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
      dst = [
        "server@:443"
        "server@:80"
      ];
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
      # Advertise certificate domains. The cached app certificate below avoids
      # public DNS/ACME; no challenge is submitted to this unused provider.
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
        stateDir = "/srv/notes/folio-data";
        authKeyFile = "/run/folio-authkey";
        logLevel = "debug";
        settings.cacheTTL = "0s";
        agents = [
          {
            tag = "tag:agent";
            actAs = "alice";
          }
          {
            tag = "tag:missing";
            actAs = "missing";
          }
        ];
      };
      systemd.services.folio = {
        wantedBy = lib.mkForce [ ];
        environment = {
          TS_CONTROL_URL = "http://headscale";
          TS_NO_LOGS_NO_SUPPORT = "1";
          SSL_CERT_FILE = "/srv/notes/folio-data/test-ca.crt";
        };
      };
      # tsnet picks an ephemeral UDP port; permit LAN discovery in this VM.
      networking.firewall.allowedUDPPortRanges = [
        {
          from = 1024;
          to = 65535;
        }
      ];
      environment.systemPackages = [
        pkgs.openssl
        pkgs.curl
        pkgs.sqlite
      ];
      virtualisation.memorySize = 2048;
    };
    alice = peer;
    bob = peer;
    agent = peer;
    relay = { ... }: {
      imports = [ peer ];
      systemd.services.tailscaled.environment.TS_DEBUG_ALWAYS_USE_DERP = "1";
    };
  };
  testScript = ''
    import json
    import shlex

    T = 120
    state = "/srv/notes/folio-data"

    def request(node, path, method="GET", data=None, status=200, headers=()):
        args = ["curl", "--silent", "--show-error", "--max-time", "10",
                "--cacert", "/tmp/app.crt", "-X", method, "-D", "/tmp/response.headers", "-w", "\\n%{http_code}"]
        for header in headers:
            args += ["-H", header]
        if data is not None:
            args += ["-H", "Content-Type: application/json", "--data", json.dumps(data)]
        args += ["https://notes.tailnet" + path]
        output = node.succeed(shlex.join(args))
        body, code = output.rsplit("\n", 1)
        assert int(code) == status, (node.name, path, code, body)
        if "data: " in body:
            body = next(line[6:] for line in body.splitlines() if line.startswith("data: "))
        return json.loads(body) if body else None

    def ready(node):
        node.wait_until_succeeds(
            "curl -fsS --max-time 5 --cacert /tmp/app.crt https://notes.tailnet/api/me", timeout=T)

    def service_route():
        routes = json.loads(machine.succeed("ip -j -4 route show default"))
        assert len(routes) == 1 and routes[0]["dev"] == lan and routes[0]["gateway"] == gateway, routes
        # Check tsnet's own route discovery inside the production service sandbox.
        machine.succeed("journalctl -u folio _PID=$(systemctl show -p MainPID --value folio) -o cat | "
                        "grep -F " + shlex.quote("defaultRoute=" + lan + " "))

    sessions = {}

    def mcp(node, method, params, session=None, denied=False):
        if method != "initialize" and node.name not in sessions:
            mcp(node, "initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                    "clientInfo": {"name": "vm-test", "version": "1"}})
        headers = ["Accept: application/json, text/event-stream", "MCP-Protocol-Version: 2025-03-26"]
        session = sessions.get(node.name) if session is None else session
        if session:
            headers += ["Mcp-Session-Id: " + session]
        result = request(node, "/mcp", "POST", {
            "jsonrpc": "2.0", "id": 1, "method": method, "params": params,
        }, headers=headers)
        assert ("error" in result) == denied, result
        if denied:
            assert "permission denied" in json.dumps(result), result
            return result["error"]
        if method == "initialize":
            response_headers = node.succeed("cat /tmp/response.headers")
            sessions[node.name] = next((line.split(":", 1)[1].strip() for line in response_headers.splitlines()
                                       if line.lower().startswith("mcp-session-id:")), "")
            headers = [h for h in headers if not h.startswith("Mcp-Session-Id:")]
            if sessions[node.name]:
                headers += ["Mcp-Session-Id: " + sessions[node.name]]
            request(node, "/mcp", "POST", {"jsonrpc": "2.0", "method": "notifications/initialized"},
                    status=202, headers=headers)
        return result["result"]

    def tool(node, name, arguments, denied=False, session=None):
        result = mcp(node, "tools/call", {"name": name, "arguments": arguments}, session=session)
        assert bool(result.get("isError")) == denied, result
        if denied:
            assert "permission denied" in json.dumps(result), result
        return result

    start_all()
    machine.wait_for_unit("multi-user.target")
    gateway = machine.succeed("getent ahostsv4 headscale | awk 'NR == 1 {print $1}'").strip()
    lan = json.loads(machine.succeed(f"ip -j route get {gateway}"))[0]["dev"]
    # Unprivileged tsnet binds to the default interface; all test peers live on the VLAN.
    machine.succeed(f"ip link set eth0 down; ip -4 route flush default; ip -6 route flush default; "
                    f"ip route add default via {gateway} dev {lan}")
    machine.succeed(f"stat -c '%U:%G %a' {state} | grep -x 'folio:folio 700'")
    headscale.succeed("umask 077; hs-authkey server > /tmp/server-key")
    headscale.succeed("headscale policy set -f ${allow}")
    keys = {user: headscale.succeed(f"hs-authkey {user}").strip() for user in ["alice", "bob"]}
    for node, user in [(alice, "alice"), (bob, "bob"), (relay, "alice")]:
        node.succeed(f"hs-join {keys[user]}")

    with subtest("real TLS certificate and invalid enrollment"):
        machine.succeed(f"install -d -o folio -g folio -m 700 {state}/tsnet/certs")
        machine.succeed(
            f"openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=notes.tailnet "
            f"-addext subjectAltName=DNS:notes.tailnet -keyout {state}/tsnet/certs/notes.tailnet.key "
            f"-out {state}/tsnet/certs/notes.tailnet.crt")
        machine.succeed(f"cp {state}/tsnet/certs/notes.tailnet.crt {state}/test-ca.crt; "
                        f"chown -R folio:folio {state}/tsnet {state}/test-ca.crt; chmod 600 {state}/tsnet/certs/*.key")
        cert = machine.succeed(f"cat {state}/test-ca.crt")
        for node in [alice, bob, relay, agent]:
            node.succeed(f"printf %s {shlex.quote(cert)} > /tmp/app.crt")
        machine.succeed("umask 077; printf %s invalid-auth-key > /run/folio-authkey; systemctl start folio")
        machine.wait_until_succeeds(
            "journalctl -u folio -o cat | grep -iE 'invalid.*key|invalid key|auth.*invalid'", timeout=T)
        nodes = json.loads(headscale.succeed("headscale nodes list -o json"))
        assert not any(n["givenName"] == "notes" for n in nodes), nodes
        machine.succeed("systemctl stop folio")
        key = headscale.succeed("cat /tmp/server-key").strip()
        machine.succeed(f"printf %s {shlex.quote(key)} > /run/folio-authkey; systemctl start folio")
        ready(alice)
        service_route()
        alice.fail("curl -fsS --max-time 5 https://notes.tailnet/api/me")
        ready(alice)
        # An application listener must not leak onto the host's LAN interface.
        assert not machine.succeed("ss -H -ltn '( sport = :80 or sport = :443 )'").strip()
        ready(alice)

    with subtest("WhoIs, user isolation and spoofed identity headers"):
        a = request(alice, "/api/me")
        b = request(bob, "/api/me")
        assert a["login"] == "alice" and b["login"] == "bob", (a, b)
        assert a["vault"] != b["vault"] and not a["isAgent"]
        assert request(relay, "/api/me")["vault"] == a["vault"]
        assert request(bob, "/api/me", headers=["X-Forwarded-For: 100.64.0.1", "Tailscale-User-Login: alice"]) == b
        request(alice, "/api/vaults/me/notes", "POST", {"path": "private.md", "content": "secretneedle"}, 201)
        request(bob, "/api/vaults/me/notes", "POST", {"path": "private.md", "content": "bobneedle"}, 201)
        path = "/api/vaults/" + a["vault"] + "/notes/private.md"
        request(bob, path, status=403)
        bob_private = request(bob, "/api/vaults/me/notes/private.md")["content"]
        assert "bobneedle" in bob_private
        assert request(bob, "/api/search?q=secretneedle")["hits"] == []
        assert [hit["path"] for hit in request(alice, "/api/search?q=secretneedle")["hits"]] == ["private.md"]
        request(bob, path, "PUT", {"content": "overwrite"}, 403)
        assert "secretneedle" in request(alice, path)["content"]
        request(alice, "/api/vaults/me/notes", "POST", {"path": "csrf.md", "content": "bad"}, 403,
                headers=["Origin: https://evil.example"])
        request(alice, "/api/vaults/me/notes", "POST", {"path": "control.md", "content": "ok"}, 201)

    with subtest("daily-note reads are read-only and creation checks origin"):
        daily = "/api/vaults/me/daily?date=2026-08-30"
        request(alice, daily + "&create=true", status=404)
        alice.succeed("test $(curl --silent --show-error --head --max-time 10 --cacert /tmp/app.crt "
                      "-o /dev/null -w '%{http_code}' https://notes.tailnet" + daily + ") = 404")
        request(alice, daily, "POST", status=403, headers=["Origin: https://evil.example"])
        request(alice, daily, "POST", status=403, headers=["Sec-Fetch-Site: cross-site"])
        machine.fail(f"test -f {state}/vaults/{a['vault']}/Daily/2026-08-30.md")
        assert machine.succeed(f"sqlite3 {state}/folio.db \"SELECT count(*) FROM notes "
                               "WHERE path = 'Daily/2026-08-30.md'\"").strip() == "0"
        created = request(alice, daily, "POST")
        assert created["path"] == "Daily/2026-08-30.md", created
        assert request(alice, daily)["content"] == created["content"]
        assert request(alice, daily, "POST")["content"] == created["content"]
        machine.succeed(f"test -f {state}/vaults/{a['vault']}/Daily/2026-08-30.md")
        assert machine.succeed(f"sqlite3 {state}/folio.db \"SELECT count(*) FROM notes "
                               "WHERE path = 'Daily/2026-08-30.md'\"").strip() == "1"

    with subtest("read, write, owner-only revoke and persistent shares"):
        grant = request(alice, "/api/shares", "POST", {"path": "private.md", "grantee": "bob", "perm": "read"}, 201)
        assert "secretneedle" in request(bob, path)["content"]
        request(bob, path, "PUT", {"content": "overwrite"}, 403)
        request(alice, path)
        request(bob, "/api/shares/" + grant["id"], "DELETE", status=403)
        request(bob, path)
        request(alice, "/api/shares/" + grant["id"], "DELETE", status=204)
        request(bob, path, status=403)
        request(bob, "/api/vaults/me/notes/private.md")
        grant = request(alice, "/api/shares", "POST", {"path": "private.md", "grantee": "bob", "perm": "write"}, 201)
        current = request(bob, path)
        request(bob, path, "PUT", {"content": "shared edit"}, headers=["If-Match: " + current["sha256"]])
        shared_content = request(alice, path)["content"]
        assert "shared edit" in shared_content

    with subtest("folder boundaries, listings and search follow grants"):
        for name in ["shared/inside.md", "sharedness/outside.md"]:
            request(alice, "/api/vaults/me/notes", "POST", {"path": name, "content": "folderneedle"}, 201)
        folder_grant = request(alice, "/api/shares", "POST",
                               {"path": "shared", "isFolder": True, "grantee": "bob", "perm": "read"}, 201)
        prefix = "/api/vaults/" + a["vault"] + "/notes/"
        request(bob, prefix + "shared/inside.md")
        request(bob, prefix + "sharedness/outside.md", status=403)
        request(bob, prefix.rstrip("/"), status=403)
        listing = json.dumps(request(alice, prefix.rstrip("/")))
        assert "shared/inside.md" in listing and "sharedness/outside.md" in listing, listing
        hits = json.dumps(request(bob, "/api/search?q=folderneedle")["hits"])
        assert "shared/inside.md" in hits and "sharedness/outside.md" not in hits, hits
        request(bob, prefix + "shared/inside.md", "DELETE", status=403)
        request(alice, prefix + "shared/inside.md")
        request(alice, "/api/vaults/me/notes", "POST", {"path": "delete-control.md", "content": "delete me"}, 201)
        request(alice, "/api/vaults/me/notes/delete-control.md", "DELETE", status=204)
        request(alice, "/api/vaults/me/notes/delete-control.md", status=404)
        request(alice, "/api/shares/" + folder_grant["id"], "DELETE", status=204)
        request(bob, prefix + "shared/inside.md", status=403)
        request(bob, "/api/vaults/me/notes/private.md")

    with subtest("tagged agent rejects unknown mapping and nonexistent user"):
        for tag in ["unmapped", "missing", "agent"]:
            key = headscale.succeed(f"hs-authkey alice --tags tag:{tag}").strip()
            agent.succeed(f"hs-join {key} --reset")
            if tag != "agent":
                # Re-enrollment updates the receiver's netmap asynchronously.
                reason = "no configured mapping" if tag == "unmapped" else "not a known user"
                agent.wait_until_succeeds(
                    "curl -sS --max-time 5 --cacert /tmp/app.crt https://notes.tailnet/api/me -o /tmp/denial; "
                    f"grep -F {shlex.quote(reason)} /tmp/denial", timeout=T)
                denial = json.dumps(request(agent, "/api/me", status=403))
                assert f"tag:{tag}" in denial, denial
                assert reason in denial, denial
                request(agent, "/mcp", "POST", {}, 403)
                request(alice, path)
                agent.succeed("tailscale logout")
            else:
                ready(agent)
                identity = request(agent, "/api/me")
                assert identity["isAgent"] and identity["vault"] == a["vault"], identity

    with subtest("MCP capabilities and shared permission enforcement"):
        init = mcp(agent, "initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                        "clientInfo": {"name": "vm-test", "version": "1"}})
        assert "tools" in init["capabilities"], init
        tools = mcp(agent, "tools/list", {})
        assert "read_note" in [t["name"] for t in tools["tools"]]
        tool(agent, "read_note", {"path": "private.md"})
        tool(agent, "create_note", {"path": "agent.md", "content": "agent write"})
        assert "agent write" in request(alice, "/api/vaults/me/notes/agent.md")["content"]
        request(alice, "/api/shares/" + grant["id"], "DELETE", status=204)
        mcp(alice, "initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                  "clientInfo": {"name": "vm-test", "version": "1"}})
        replay = sessions[alice.name] or "foreign-session-id"
        tool(bob, "read_note", {"vault": a["vault"], "path": "private.md"}, denied=True, session=replay)
        own = json.dumps(tool(bob, "read_note", {"path": "private.md"}, session=replay))
        assert "bobneedle" in own and "shared edit" not in own, own
        resource = mcp(bob, "resources/read", {"uri": "folio://me/private.md"}, session=replay)
        assert resource["contents"][0]["text"] == bob_private, resource
        mcp(bob, "resources/read", {"uri": "folio://" + a["vault"] + "/private.md"}, session=replay, denied=True)
        tool(bob, "create_note", {"path": "bob-mcp.md", "content": "Bob MCP write"}, session=replay)
        assert "Bob MCP write" in request(bob, "/api/vaults/me/notes/bob-mcp.md")["content"]
        assert "shared edit" in json.dumps(tool(alice, "read_note", {"path": "private.md"}))
        grant = request(alice, "/api/shares", "POST", {"path": "private.md", "grantee": "bob", "perm": "read"}, 201)
        tool(bob, "read_note", {"vault": a["vault"], "path": "private.md"})
        tool(bob, "append_note", {"vault": a["vault"], "path": "private.md", "text": "bad"}, denied=True)
        assert request(alice, path)["content"] == shared_content
        tool(agent, "append_note", {"path": "agent.md", "text": "allowed append"})
        assert "allowed append" in request(alice, "/api/vaults/me/notes/agent.md")["content"]

    with subtest("direct and forced DERP application traffic"):
        alice.wait_until_succeeds("tailscale ping --timeout 3s -c 1 notes", timeout=T)
        alice.succeed("tailscale status --json | jq -e '.Peer[] | select(.HostName == \"notes\") | .CurAddr != \"\"'")
        request(alice, path)
        relay.succeed("tailscale ping --until-direct=false --timeout 5s -c 3 notes | grep 'via DERP(headscale)'")
        request(relay, "/api/vaults/me/notes", "POST", {"path": "relay.md", "content": "DERP write"}, 201)
        assert "DERP write" in request(alice, "/api/vaults/me/notes/relay.md")["content"]
        tool(relay, "read_note", {"path": "relay.md"})

    with subtest("network ACL denies HTTPS while HTTP redirect stays allowed"):
        headscale.succeed("headscale policy set -f ${restricted}")
        blocked_https = (
            "code=0; http=$(curl -sS --max-time 3 --cacert /tmp/app.crt -o /dev/null "
            "-w '%{http_code}' https://notes.tailnet/api/me) || code=$?; "
            "test \"$code:$http\" = '28:000'")
        bob.wait_until_succeeds(blocked_https, timeout=T)
        bob.succeed("curl -sS --max-time 5 -D /tmp/headers http://notes.tailnet/api/me -o /dev/null; "
                    "grep '301' /tmp/headers; grep -i 'location: https://notes.tailnet/api/me' /tmp/headers")
        request(alice, path)
        bob.succeed(blocked_https)
        headscale.succeed("headscale policy set -f ${allow}")
        ready(bob)

    with subtest("control outage retains established identity and storage"):
        bob.wait_until_succeeds("tailscale ping --timeout 3s -c 1 notes", timeout=T)
        bob.wait_until_succeeds("tailscale status --json | jq -e '.Peer[] | select(.HostName == \"notes\") | .CurAddr != \"\"'", timeout=T)
        headscale.succeed("systemctl stop headscale")
        request(alice, "/api/vaults/me/notes", "POST", {"path": "offline.md", "content": "offline control"}, 201)
        request(bob, "/api/vaults/" + a["vault"] + "/notes/offline.md", status=403)
        request(bob, path)
        headscale.succeed("systemctl start headscale; HEADSCALE_CLI_TIMEOUT=60s headscale health")
        assert "offline control" in request(alice, "/api/vaults/me/notes/offline.md")["content"]

    with subtest("restart without auth key preserves node, identity, notes and shares"):
        before = json.loads(headscale.succeed("headscale nodes list -o json"))
        machine.succeed("systemctl stop folio; : > /run/folio-authkey; systemctl start folio")
        ready(alice)
        service_route()
        after = json.loads(headscale.succeed("headscale nodes list -o json"))
        def node_id(nodes):
            return [(n["id"], n["ipAddresses"]) for n in nodes if n["givenName"] == "notes"]
        assert node_id(before) and node_id(before) == node_id(after), (before, after)
        assert request(alice, "/api/me") == a
        assert "shared edit" in request(bob, path)["content"]
        request(bob, path, "PUT", {"content": "bad"}, 403)
        request(alice, path)
        machine.succeed(f"stat -c '%U:%G %a' {state} | grep -x 'folio:folio 700'")
        machine.succeed("test $(systemctl show -p User --value folio) = folio; "
                        "test -z \"$(systemctl show -p CapabilityBoundingSet --value folio)\"")
        machine.fail("journalctl -u folio | grep -iE 'bad system call|SIGSYS'")
  '';
}
