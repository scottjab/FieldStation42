# FieldStation42 JSON Conversion

This document explains the JSON conversion process for FieldStation42's pickle schedule and catalog files.

## Overview

The Go web player now uses JSON files instead of Python pickle files for better compatibility and easier parsing. A Python conversion script (`convert_schedules.py`) converts the pickle files to JSON format.

## How It Works

1. **Conversion Script**: `convert_schedules.py` reads all station configurations and converts their pickle schedule and catalog files to JSON format.

2. **Automatic Conversion**: The nix configuration automatically runs the conversion script before starting the web player.

3. **JSON Format**: The converted files are stored in the `json_schedules/` directory with the naming pattern:
   - `{network_name}_schedule.json` for schedule files
   - `{network_name}_catalog.json` for catalog files

## Usage

### Manual Conversion

To manually convert pickle files to JSON:

```bash
# Convert all schedules and catalogs
python3 convert_schedules.py

# Or use the nix wrapper
convert_schedules
```

### Automatic Conversion

The web player automatically runs the conversion when started:

```bash
# This will convert pickle files to JSON, then start the web player
web_field_player
```

## File Structure

After conversion, you'll have:

```
json_schedules/
├── Guide_schedule.json
├── Guide_catalog.json
├── FieldStation42_schedule.json
├── FieldStation42_catalog.json
└── ... (other station files)
```

## JSON Format

### Schedule Files

```json
[
  {
    "start_time": {
      "__type__": "datetime",
      "value": "2024-01-01T07:00:00"
    },
    "end_time": {
      "__type__": "datetime", 
      "value": "2024-01-01T08:00:00"
    },
    "title": "Morning Block",
    "plan": [
      {
        "path": "/path/to/video.mp4",
        "duration": 1800,
        "skip": 0,
        "is_stream": false
      }
    ]
  }
]
```

### Catalog Files

```json
{
  "version": 0.1,
  "clip_index": {
    "content": [
      {
        "path": "/path/to/video.mp4",
        "title": "Video Title",
        "duration": 1800.0,
        "tag": "content",
        "count": 1,
        "hints": ["morning", "family"]
      }
    ]
  },
  "sequences": {
    "series_name": {
      "episodes": ["/path/to/ep1.mp4", "/path/to/ep2.mp4"],
      "current_index": 0
    }
  }
}
```

## Troubleshooting

### No JSON Files Created

If no JSON files are created, check:

1. **Pickle files exist**: Ensure your station configurations have valid `schedule_path` and `catalog_path` entries
2. **File permissions**: Make sure the script can read the pickle files
3. **Python dependencies**: Ensure all required Python modules are available

### Web Player Can't Find JSON Files

If the web player can't find JSON files:

1. **Run conversion first**: Execute `convert_schedules` before starting the web player
2. **Check file paths**: Verify JSON files are in the `json_schedules/` directory
3. **Station names**: Ensure the JSON filenames match your station network names

### Conversion Errors

Common conversion errors:

- **Missing pickle files**: The script will skip stations without pickle files
- **Corrupted pickle files**: Check the original pickle files for corruption
- **Permission issues**: Ensure read access to pickle files and write access to `json_schedules/` directory

## Benefits

1. **Better Performance**: JSON parsing is faster than pickle parsing in Go
2. **Cross-Platform**: JSON is more portable than Python pickle files
3. **Debugging**: JSON files are human-readable and easier to debug
4. **Compatibility**: No need for complex pickle parsing libraries in Go 