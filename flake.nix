{
  description = "cuttle - host CLI for the stealth-Chromium CDP farm";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # The CLI is pure Python, so it builds from source on every platform -
      # unlike the Docker image (the actual browser farm), which is amd64-only.
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = (builtins.fromTOML (builtins.readFile ./pyproject.toml)).project.version;
      mkCuttle =
        pkgs:
        pkgs.python312.pkgs.buildPythonApplication {
          pname = "cuttle-browser";
          inherit version;
          pyproject = true;
          src = self;
          build-system = [ pkgs.python312.pkgs.setuptools ];
          dependencies = with pkgs.python312.pkgs; [
            aiohttp
            websockets
            httpx
            geoip2
            socksio
          ];
          # The test harness needs a running cuttle container; nothing to run here.
          doCheck = false;
          pythonImportsCheck = [ "cuttle" ];
          meta = {
            description = "Host CLI for the cuttle stealth-Chromium CDP farm";
            homepage = "https://github.com/glim-sh/cuttle";
            changelog = "https://github.com/glim-sh/cuttle/releases/tag/v${version}";
            license = nixpkgs.lib.licenses.mit;
            mainProgram = "cuttle";
          };
        };
    in
    {
      overlays.default = final: _prev: { cuttle = mkCuttle final; };

      packages = forAllSystems (pkgs: rec {
        cuttle = mkCuttle pkgs;
        default = cuttle;
      });

      apps = forAllSystems (pkgs: rec {
        cuttle = {
          type = "app";
          program = "${self.packages.${pkgs.system}.cuttle}/bin/cuttle";
          meta.description = "Run the cuttle host CLI";
        };
        default = cuttle;
      });
    };
}
