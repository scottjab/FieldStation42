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
          python ? nixpkgs.legacyPackages.${system}.python311Full,
        }:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          pythonPackages = python.pkgs;

          # Helper function to disable tests for a package
          disableTests =
            pkg:
            pkg.overridePythonAttrs (old: {
              doCheck = false;
              doInstallCheck = false;
            });

          # Override problematic packages
          overriddenPython = python.override {
            packageOverrides = self: super: {
              pytest-doctestplus = super.pytest-doctestplus.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              pyerfa = super.pyerfa.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              astropy = super.astropy.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              imageio = super.imageio.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              watchdog = super.watchdog.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              werkzeug = super.werkzeug.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              flask = super.flask.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              httpbin = super.httpbin.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              fastapi = super.fastapi.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              pytest-httpbin = super.pytest-httpbin.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              tqdm = super.tqdm.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              charset-normalizer = super.charset-normalizer.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              pproxy = super.pproxy.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
              starlette = super.starlette.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
            };
          };

          # Create a Python environment with all required packages
          pythonEnv = overriddenPython.withPackages (
            ps: with ps; [
              # Core dependencies from requirements.txt
              fastapi
              ffmpeg-python
              (disableTests (
                moviepy.overridePythonAttrs (old: {
                  buildInputs = (old.buildInputs or [ ]) ++ [ ps.numpy ];
                })
              ))
              pyserial
              python-mpv-jsonipc
              (disableTests textual)
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
            export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
            export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
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
              tcl
              tk
              pkg-config
              gcc
              libffi
              openssl
              sqlite
              xz
              zlib
              gdbm
              readline
              ncurses
              bzip2
              expat
              mpdecimal
              tzdata
            ];

            shellHook = ''
              # Set up environment variables
              export PYTHONPATH=$PWD:$PYTHONPATH
              export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
              export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
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
