#!/usr/bin/env python3
"""
Convert FieldStation42 pickle schedule files to JSON format for the Go web player.
This script reads the pickle schedule files and converts them to a JSON format that
the Go web player can easily parse.
"""

import os
import sys
import json
import pickle
import datetime
from pathlib import Path

# Add the current directory to Python path to import fs42 modules
sys.path.append(os.getcwd())

from fs42.station_manager import StationManager
from fs42.liquid_manager import LiquidManager


def datetime_to_json(obj):
    """Convert datetime objects to JSON-serializable format."""
    if isinstance(obj, datetime.datetime):
        return {
            "__type__": "datetime",
            "value": obj.isoformat()
        }
    elif isinstance(obj, datetime.date):
        return {
            "__type__": "date", 
            "value": obj.isoformat()
        }
    elif isinstance(obj, datetime.timedelta):
        return {
            "__type__": "timedelta",
            "value": obj.total_seconds()
        }
    return obj


def convert_schedule_to_json(network_name, schedule_path, output_path):
    """Convert a single schedule file from pickle to JSON."""
    print(f"Converting schedule for {network_name} from {schedule_path}")
    
    try:
        # Load the pickle schedule
        with open(schedule_path, 'rb') as f:
            schedule_blocks = pickle.load(f)
        
        # Convert schedule blocks to JSON-serializable format
        json_schedule = []
        for block in schedule_blocks:
            block_data = {
                "start_time": datetime_to_json(block.start_time),
                "end_time": datetime_to_json(block.end_time),
                "title": block.title,
                "plan": []
            }
            
            # Convert plan entries
            for entry in block.plan:
                entry_data = {
                    "path": entry.path,
                    "duration": entry.duration,
                    "skip": entry.skip,
                    "is_stream": getattr(entry, 'is_stream', False)
                }
                block_data["plan"].append(entry_data)
            
            json_schedule.append(block_data)
        
        # Write JSON file
        with open(output_path, 'w') as f:
            json.dump(json_schedule, f, indent=2, default=datetime_to_json)
        
        print(f"Successfully converted {len(json_schedule)} blocks to {output_path}")
        return True
        
    except Exception as e:
        print(f"Error converting schedule for {network_name}: {e}")
        return False


def convert_catalog_to_json(network_name, catalog_path, output_path):
    """Convert a single catalog file from pickle to JSON."""
    print(f"Converting catalog for {network_name} from {catalog_path}")
    
    try:
        # Load the pickle catalog
        with open(catalog_path, 'rb') as f:
            catalog_data = pickle.load(f)
        
        # Convert catalog to JSON-serializable format
        json_catalog = {
            "version": catalog_data.get("version", 0.1),
            "clip_index": {},
            "sequences": {}
        }
        
        # Convert clip_index
        for tag, entries in catalog_data.get("clip_index", {}).items():
            if isinstance(entries, list):
                json_entries = []
                for entry in entries:
                    if hasattr(entry, 'path'):
                        entry_data = {
                            "path": entry.path,
                            "title": getattr(entry, 'title', ''),
                            "duration": getattr(entry, 'duration', 0),
                            "tag": getattr(entry, 'tag', ''),
                            "count": getattr(entry, 'count', 0),
                            "hints": getattr(entry, 'hints', [])
                        }
                        json_entries.append(entry_data)
                json_catalog["clip_index"][tag] = json_entries
            else:
                json_catalog["clip_index"][tag] = entries
        
        # Convert sequences
        for seq_key, sequence in catalog_data.get("sequences", {}).items():
            if hasattr(sequence, 'episodes'):
                seq_data = {
                    "episodes": [ep.fpath for ep in sequence.episodes],
                    "current_index": getattr(sequence, 'current_index', 0)
                }
                json_catalog["sequences"][seq_key] = seq_data
            else:
                json_catalog["sequences"][seq_key] = str(sequence)
        
        # Write JSON file
        with open(output_path, 'w') as f:
            json.dump(json_catalog, f, indent=2, default=datetime_to_json)
        
        print(f"Successfully converted catalog to {output_path}")
        return True
        
    except Exception as e:
        print(f"Error converting catalog for {network_name}: {e}")
        return False


def main():
    """Convert all schedule and catalog files to JSON."""
    print("Converting FieldStation42 pickle files to JSON...")
    
    # Create output directory
    json_dir = Path("json_schedules")
    json_dir.mkdir(exist_ok=True)
    
    # Get all stations
    station_manager = StationManager()
    success_count = 0
    total_count = 0
    
    for station in station_manager.stations:
        network_name = station["network_name"]
        total_count += 1
        
        print(f"\nProcessing station: {network_name}")
        
        # Convert schedule if it exists
        schedule_path = station.get("schedule_path")
        if not schedule_path:
            # Try common schedule path patterns
            possible_paths = [
                f"runtime/{network_name}.bin",
                f"runtime/{network_name}_schedule.bin",
                f"runtime/{network_name.lower()}.bin",
                f"runtime/{network_name.lower()}_schedule.bin"
            ]
            for path in possible_paths:
                if os.path.exists(path):
                    schedule_path = path
                    print(f"Found schedule at: {schedule_path}")
                    break
        
        if schedule_path and os.path.exists(schedule_path):
            json_schedule_path = json_dir / f"{network_name}_schedule.json"
            if convert_schedule_to_json(network_name, schedule_path, json_schedule_path):
                success_count += 1
        else:
            print(f"No schedule file found for {network_name}")
        
        # Convert catalog if it exists
        catalog_path = station.get("catalog_path")
        if not catalog_path:
            # Try common catalog path patterns
            possible_paths = [
                f"catalog/{network_name}.bin",
                f"catalog/{network_name}_catalog.bin",
                f"catalog/{network_name.lower()}.bin",
                f"catalog/{network_name.lower()}_catalog.bin"
            ]
            for path in possible_paths:
                if os.path.exists(path):
                    catalog_path = path
                    print(f"Found catalog at: {catalog_path}")
                    break
        
        if catalog_path and os.path.exists(catalog_path):
            json_catalog_path = json_dir / f"{network_name}_catalog.json"
            if convert_catalog_to_json(network_name, catalog_path, json_catalog_path):
                success_count += 1
        else:
            print(f"No catalog file found for {network_name}")
    
    print(f"\nConversion complete: {success_count} files converted successfully out of {total_count} stations")
    print(f"JSON files saved to: {json_dir.absolute()}")
    
    # List the created JSON files
    if json_dir.exists():
        json_files = list(json_dir.glob("*.json"))
        if json_files:
            print("\nCreated JSON files:")
            for json_file in json_files:
                print(f"  - {json_file.name}")
        else:
            print("\nNo JSON files were created (no pickle files found)")


if __name__ == "__main__":
    main() 