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
          python ? nixpkgs.legacyPackages.${system}.python313Full,
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
              black = super.black.overridePythonAttrs (old: {
                doCheck = false;
                doInstallCheck = false;
              });
            };
          };

          # Create a Python environment with all required packages
          pythonEnv = overriddenPython.withPackages (
            ps: with ps; [
              # Core dependencies from requirements.txt
              ffmpeg-python
              (disableTests (
                moviepy.overridePythonAttrs (old: {
                  buildInputs = (old.buildInputs or [ ]) ++ [ ps.numpy ];
                })
              ))
              pyserial
              python-mpv-jsonipc
              (disableTests textual)

              # Development tools
              black
              flake8
              mypy
            ]
          );

          # Build the Go web field player binary
          webFieldPlayerGo = pkgs.buildGoModule {
            pname = "web-field-player";
            version = "1.0.0";
            src = self;
            vendorHash = null; # Disable vendoring
            doCheck = false;
          };

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

          # Go web field player binary wrapper
          webFieldPlayer = pkgs.writeScriptBin "web_field_player" ''
            #!${pkgs.bash}/bin/bash
            # Set up Python environment for convert_schedules.py
            export PYTHONPATH=${self}:$PYTHONPATH
            export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
            export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
            
            # Convert schedules to JSON before starting the web player
            echo "Converting schedules to JSON format..."
            ${pythonEnv}/bin/python ${self}/convert_schedules.py
            
            # Set up GStreamer environment
            export GST_PLUGIN_PATH=${pkgs.gst_all_1.gstreamer}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-base}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-good}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-bad}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-ugly}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-libav}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-vaapi}/lib/gstreamer-1.0
            export GST_PLUGIN_SYSTEM_PATH=${pkgs.gst_all_1.gstreamer}/lib/gstreamer-1.0
            export GST_REGISTRY_FORK=no
            export PATH=${pkgs.gst_all_1.gst-devtools}/bin:$PATH
            
            echo "Starting web field player..."
            ${webFieldPlayerGo}/bin/fieldstation42 "$@"
          '';
        in
        {
          packages = {
            field-player = fieldPlayer;
            station = station;
            web-field-player = webFieldPlayer;
            web-field-player-go = webFieldPlayerGo;
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
            web-field-player = {
              type = "app";
              program = "${webFieldPlayer}/bin/web_field_player";
            };
            default = self.apps.${system}.station;
          };

          devShell = pkgs.mkShell {
            buildInputs = with pkgs; [
              # Python environment with all dependencies
              pythonEnv

              # Go for development
              go

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
              # FFmpeg with QuickSync support
              (ffmpeg.override {
                nvenc = true;
                vaapi = true;
                vdpau = true;
                # Enable QuickSync (Intel Quick Sync Video)
                extraOptions = {
                  qsv = true;
                };
              })

              # GStreamer for video streaming
              gst_all_1.gstreamer
              gst_all_1.gst-plugins-base
              gst_all_1.gst-plugins-good
              gst_all_1.gst-plugins-bad
              gst_all_1.gst-plugins-ugly
              gst_all_1.gst-libav
              gst_all_1.gst-vaapi
              gst_all_1.gst-devtools
              gst_all_1.gst-validate
            ];

            shellHook = ''
              # Set up environment variables
              export PYTHONPATH=$PWD:$PYTHONPATH
              export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
              export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
              
              # GStreamer environment variables
              export GST_PLUGIN_PATH=${pkgs.gst_all_1.gstreamer}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-base}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-good}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-bad}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-plugins-ugly}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-libav}/lib/gstreamer-1.0:${pkgs.gst_all_1.gst-vaapi}/lib/gstreamer-1.0
              export GST_PLUGIN_SYSTEM_PATH=${pkgs.gst_all_1.gstreamer}/lib/gstreamer-1.0
              export GST_REGISTRY_FORK=no
              export PATH=${pkgs.gst_all_1.gst-devtools}/bin:${pkgs.gst_all_1.gst-validate}/bin:$PATH
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
