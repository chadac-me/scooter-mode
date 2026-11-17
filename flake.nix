{
  description = "Scooter from Emacs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    systems.url = "github:nix-systems/default";
    scooter-cli.url = "github:chadac-me/scooter-cli";
    scooter-cli.inputs.nixpkgs.follows = "nixpkgs";
    scooter-cli.inputs.flake-parts.follows = "flake-parts";
    scooter-cli.inputs.systems.follows = "systems";
  };

  outputs = inputs@{ flake-parts, systems, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = import systems;

      perSystem = { pkgs, self', inputs', ... }: {
        # The bridge comes from scooter-cli; re-exported for `nix profile install`.
        packages.scooter = inputs'.scooter-cli.packages.scooter;
        packages.default = self'.packages.scooter;

        devShells.default = pkgs.mkShell {
          # Cask runs a bare Emacs so MELPA, not nixpkgs, supplies the deps.
          CASK_EMACS = "${pkgs.emacs-nox}/bin/emacs";
          packages = [
            (pkgs.emacs-nox.pkgs.withPackages (e: [ e.magit-section e.transient e.agent-shell e.evil e.projectile ]))
            pkgs.emacs-nox.pkgs.cask
            pkgs.just
            pkgs.curl
          ];
        };
      };
    };
}
