{
  pkgs ? import <nixpkgs> { },
  python ? pkgs.python311,
}:

let
  pythonPackages = python.pkgs;

  # Helper function to disable tests for a package
  disableTests =
    pkg:
    pkg.overridePythonAttrs (old: {
      doCheck = false;
      doInstallCheck = false;
    });

  # Create a Python environment with all required packages
  pythonEnv = python.withPackages (
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
    src = ./.;
    vendorHash = null; # Disable vendoring
    doCheck = false;
  };

  # Create wrapper scripts for the Python applications
  fieldPlayer = pkgs.writeScriptBin "field_player" ''
    #!${pkgs.bash}/bin/bash
    export PYTHONPATH=${./.}:$PYTHONPATH
    export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
    export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
    ${pythonEnv}/bin/python ${./field_player.py} "$@"
  '';

  station = pkgs.writeScriptBin "station_42" ''
    #!${pkgs.bash}/bin/bash
    export PYTHONPATH=${./.}:$PYTHONPATH
    ${pythonEnv}/bin/python ${./station_42.py} "$@"
  '';

  # Go web field player binary wrapper
  webFieldPlayer = pkgs.writeScriptBin "web_field_player" ''
    #!${pkgs.bash}/bin/bash
    ${webFieldPlayerGo}/bin/web-field-player "$@"
  '';
in

{
  # The main package
  fieldstation42 = pkgs.symlinkJoin {
    name = "fieldstation42";
    paths = [
      fieldPlayer
      station
      webFieldPlayer
    ];
  };

  # Individual components
  inherit fieldPlayer station webFieldPlayer webFieldPlayerGo;

  # Development environment
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
    ];

    shellHook = ''
      # Set up environment variables
      export PYTHONPATH=$PWD:$PYTHONPATH
      export TCL_LIBRARY=${pkgs.tcl}/lib/tcl8.6
      export TK_LIBRARY=${pkgs.tk}/lib/tk8.6
    '';
  };
}
