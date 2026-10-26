{
  description = "folio: a self-hosted, tailnet-native markdown notes app";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # go.mod requires go >= 1.27.3. go_1_27 in a floating nixpkgs is only as
    # new as its latest patch release, so a stale lock (or a consumer that
    # follows its own nixpkgs) silently ships an older go and fails the build
    # with a confusing go.mod error. Pin the toolchain to a revision that is
    # known to provide go 1.27.3 and bring it in as its own dependency.
    go-nixpkgs.url = "github:NixOS/nixpkgs/02c087f3fe673c9e317f206a627cb6b910336dbe";
    headscale.url = "github:juanfont/headscale/2d220b2eb8be8d31ed603000d0b0be02c2cd94d0";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, go-nixpkgs, flake-utils, headscale }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        goPkgs = import go-nixpkgs { inherit system; };
        inherit (nixpkgs) lib;
        go = goPkgs.go_1_27;
        buildGoModule = pkgs.buildGoModule.override { inherit go; };

        # Release number, bumped when cutting a release.
        version = "0.1.3";

        # What the binary reports. Nix flakes cannot see git tags or run git,
        # so there is no git-describe here; instead we append the source's own
        # metadata, which is enough to trace any build back to a commit. A
        # tarball build has neither rev nor dirtyRev and just gets the release
        # number.
        sourceRev = self.rev or self.dirtyRev or null;
        buildVersion = if sourceRev == null
          then version
          else "${version}-${self.lastModifiedDate}-${lib.substring 0 12 sourceRev}";

        frontend = pkgs.buildNpmPackage {
          pname = "folio-frontend";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-RPj+NvTN/z/DEvbt+rwN2CmiCxe+zX4bmn8xu7y0dks=";
          dontNpmInstall = true;
          installPhase = ''
            runHook preInstall
            mkdir -p $out
            cp -r dist $out/dist
            runHook postInstall
          '';
        };

        folio = buildGoModule {
          pname = "folio";
          inherit version;
          src = ./.;
          vendorHash = "sha256-xXgUU/YS7a5qLtsGXv/M01TD1ofTQQKaiIHda9wIWzc=";

          env.CGO_ENABLED = 0;

          # The Go build embeds internal/web/dist. A placeholder is committed so
          # `go test ./...` works without node; here we swap in the real bundle.
          preBuild = ''
            cp -r ${frontend}/dist/. internal/web/dist/
            chmod -R u+w internal/web/dist
            # Fail here rather than shipping a binary whose UI is a stub page.
            test -f internal/web/dist/app.js \
              || (echo "frontend bundle missing after copy" >&2; exit 1)
          '';

          ldflags = [ "-s" "-w" "-X" "main.version=${buildVersion}" ];

          subPackages = [ "cmd/folio" ];

          meta = with pkgs.lib; {
            description = "Self-hosted, tailnet-native markdown notes app with WYSIWYG editing and MCP";
            mainProgram = "folio";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          inherit frontend folio;
          default = folio;
        };

        apps.default = flake-utils.lib.mkApp { drv = folio; };

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            (pkgs.go-tools.override { inherit buildGoModule; })
            pkgs.golangci-lint
            pkgs.delve
            pkgs.nodejs_24
            pkgs.esbuild
            pkgs.prefetch-npm-deps
            pkgs.sqlite
            pkgs.tailscale
            pkgs.jq
            # For `make icons`, which re-renders the app's PNG icons from their
            # SVG sources. Not part of any build: the PNGs are committed.
            pkgs.imagemagick
          ];

          shellHook = ''
            export GOTOOLCHAIN=local
            echo "folio dev shell: $(go version)"
          '';
        };

        checks = {
          # buildGoModule checks the selected command package. `make check`
          # separately runs the full Go and frontend suites.
          build = folio;
          inherit frontend;

          # Instantiates the NixOS module for real. Without this nothing ever
          # evaluates it, and a broken option type or an ExecStart that does not
          # resolve would only surface on someone's machine at deploy time.
          nixos-module = (nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              ({ lib, ... }: {
                boot.loader.grub.enable = false;
                fileSystems."/" = { device = "/dev/null"; fsType = "ext4"; };
                system.stateVersion = "25.05";
                nixpkgs.hostPlatform = system;

                services.folio = {
                  enable = true;
                  hostname = "notes";
                  authKeyFile = "/run/secrets/folio-authkey";
                  agents = [{ tag = "tag:notes-agent"; actAs = "you@github"; }];
                  settings.cacheTTL = "60s";
                };
              })
            ];
          }).config.system.build.toplevel;
        }
        # Boots a machine and starts the service. Evaluating the module proves
        # the options are well formed; only running it proves the systemd
        # hardening does not stop folio dead, which is the failure that would
        # otherwise turn up on a real deploy.
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          nixos-vm = import ./nix/e2e.nix { inherit pkgs self headscale; };
        };
      })
    // {
      nixosModules.default = import ./nix/module.nix { inherit self; };
      nixosModules.folio = self.nixosModules.default;
    };
}
