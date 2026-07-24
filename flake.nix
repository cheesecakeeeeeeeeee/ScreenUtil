{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.flake-utils.url = "github:numtide/flake-utils";

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "goshot";
          version = "0.1.0";
          src = ./.;
          vendorHash = null;

          ldflags = [
            "-s"
            "-w"
          ];

          meta = {
            description = "Wayland screenshot/region selection utility";
            mainProgram = "screenutil";
            platforms = pkgs.lib.platforms.linux;
          };
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
          ];
        };
      }
    );
}
