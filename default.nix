{ pkgs ? import <nixpkgs> {}
, python ? pkgs.python3Full
}:

let
  pythonPackages = python.pkgs;
  
  # Create a Python environment with all required packages
  pythonEnv = python.withPackages (ps: with ps; [
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
  ]);

  # Create wrapper scripts for the Python applications
  fieldPlayer = pkgs.writeScriptBin "field_player" ''
    #!${pkgs.bash}/bin/bash
    export PYTHONPATH=${./.}:$PYTHONPATH
    ${pythonEnv}/bin/python ${./field_player.py} "$@"
  '';

  station = pkgs.writeScriptBin "station_42" ''
    #!${pkgs.bash}/bin/bash
    export PYTHONPATH=${./.}:$PYTHONPATH
    ${pythonEnv}/bin/python ${./station_42.py} "$@"
  '';
in

{
  # The main package
  fieldstation42 = pkgs.symlinkJoin {
    name = "fieldstation42";
    paths = [ fieldPlayer station ];
  };

  # Individual components
  inherit fieldPlayer station;

  # Development environment
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
} 