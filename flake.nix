{
  description = "End-to-end stack for WebRTC";

  inputs = {
    nixpkgs.url = "nixpkgs/nixos-unstable";
    utils.url = "github:numtide/flake-utils";

    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, utils, gomod2nix }:
    utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ gomod2nix.overlays.default ];
        };
      in {
        packages.default = pkgs.buildGoApplication {
          pname = "livekit-server";
          version = "1.5.2";
          src = ./.;
          subPackages = "cmd/server";
          modules = ./gomod2nix.toml;
          postInstall = ''
            mv $out/bin/{,livekit-}server
          '';
        };
        devShells.default = with pkgs; mkShell {
          buildInputs = [
            go
            gopls
            gomod2nix.packages.${system}.default
            mage
          ];
        };
      }
    );
}
