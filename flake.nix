{
  description = "FieldStation42 Development Environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      # Make the package configuration available to other flakes
      mkFieldStation =
        {
          system,
          python ? nixpkgs.legacyPackages.${system}.python3Full,
        }:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          pythonPackages = python.pkgs;

          # Create a Python environment with all required packages
          pythonEnv = python.withPackages (
            ps: with ps; [
              # Core dependencies from requirements.txt
              fastapi
              ffmpeg-python
              moviepy
              pyserial
              python-mpv-jsonipc
              textual
              uvicorn

              # Development tools
              black
              flake8
              mypy
            ]
          );

          # Create wrapper scripts for the Python applications
          fieldPlayer = pkgs.writeScriptBin "field_player" ''
            #!${pkgs.bash}/bin/bash
            export PYTHONPATH=${self}:$PYTHONPATH
            ${pythonEnv}/bin/python ${self}/field_player.py "$@"
          '';

          station = pkgs.writeScriptBin "station_42" ''
            #!${pkgs.bash}/bin/bash
            export PYTHONPATH=${self}:$PYTHONPATH
            ${pythonEnv}/bin/python ${self}/station_42.py "$@"
          '';
        in
        {
          packages = {
            field-player = fieldPlayer;
            station = station;
            default = station;
          };

          apps = {
            field-player = {
              type = "app";
              program = "${fieldPlayer}/bin/field_player";
            };
            station = {
              type = "app";
              program = "${station}/bin/station_42";
            };
            default = self.apps.${system}.station;
          };

          devShell = pkgs.mkShell {
            buildInputs = with pkgs; [
              # Python environment with all dependencies
              pythonEnv

              # System dependencies
              ffmpeg
              mpv
            ];

            shellHook = ''
              # Set up environment variables
              export PYTHONPATH=$PWD:$PYTHONPATH
            '';
          };
        };
    in
    flake-utils.lib.eachDefaultSystem (system: mkFieldStation { inherit system; })
    // {
      # Expose the package configuration for other flakes
      lib = {
        inherit mkFieldStation;
      };
    };
}
