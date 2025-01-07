{
  description = "urnetwork server";
  inputs = {

    autobeam = {
      url = "github:draganm/autobeam";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    nixpkgs = { url = "github:NixOS/nixpkgs/nixos-24.11"; };

    systems.url = "github:nix-systems/default";

  };

  outputs = { self, nixpkgs, systems, autobeam, ... }@inputs:
    let
      eachSystem = f:
        nixpkgs.lib.genAttrs (import systems) (system:
          f (import nixpkgs {
            inherit system;
            config = { allowUnfree = true; };
          }));

    in {
      devShells = eachSystem (pkgs:
        let autobeam_ = autobeam.packages.${pkgs.system}.default;
        in {
          default = pkgs.mkShell {
            shellHook = ''
              # Set here the env vars you want to be available in the shell
            '';
            hardeningDisable = [ "all" ];

            packages =
              [ autobeam_ pkgs.go pkgs.tmux pkgs.nodejs_20 pkgs.python39Full ];
          };
        });
    };
}

