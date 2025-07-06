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

  # Schedule conversion script
  convertSchedules = pkgs.writeScriptBin "convert_schedules" ''
    #!${pkgs.bash}/bin/bash
    export PYTHONPATH=${./.}:$PYTHONPATH
    ${pythonEnv}/bin/python ${./convert_schedules.py} "$@"
  '';

  # Go web field player binary wrapper with conversion
  webFieldPlayer = pkgs.writeScriptBin "web_field_player" ''
    #!${pkgs.bash}/bin/bash
    # Convert pickle schedules to JSON first
    echo "Converting pickle schedules to JSON..."
    export PYTHONPATH=${./.}:$PYTHONPATH
    ${pythonEnv}/bin/python ${./convert_schedules.py}
    
    # Start the web player
    echo "Starting web field player..."
    ${webFieldPlayerGo}/bin/fieldstation42 "$@"
  '';
in

{
  # The main package
  fieldstation42 = pkgs.symlinkJoin {
    name = "fieldstation42";
    paths = [
      fieldPlayer
      station
      convertSchedules
      webFieldPlayer
    ];
  };

  # Individual components
  inherit fieldPlayer station convertSchedules webFieldPlayer webFieldPlayerGo;

  # Export packages with the names that the host configuration expects
  packages = {
    fieldPlayer = fieldPlayer;
    station = station;
    convertSchedules = convertSchedules;
    webFieldPlayer = webFieldPlayer;
    web-field-player = webFieldPlayer;
    convert_schedules = convertSchedules;
    field-player = fieldPlayer;
    station_42 = station;
  };

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
