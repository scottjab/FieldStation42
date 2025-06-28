import logging
import sqlite3
import sys
import os

sys.path.append(os.getcwd())

from fs42.fluid_statements import FluidStatements
from fs42.media_processor import MediaProcessor
from fs42.station_manager import StationManager

class FluidBuilder:
    def __init__(self, db_path=None):
        if db_path is None:
            self.db_path = StationManager().server_conf["db_path"]

        self._l = logging.getLogger("FLUID")
        with sqlite3.connect(self.db_path) as connection:
            FluidStatements.init_db(connection)

    def scan_file_cache(self, content_dir):
        with sqlite3.connect(self.db_path) as connection:
            # read all the files in the content dir
            self._l.info(f"Fluid file cache scan - reading {content_dir}")
            file_list = MediaProcessor.rich_find_media(content_dir)
            self._l.info(f"Comparing cache against {len(file_list)} files")
            # add any that aren't there yet
            FluidStatements.iterate_file_entries(connection, file_list)
            self._l.info("Checking file meta for stale entries.")

    def check_file_cache(self, full_path):
        with sqlite3.connect(self.db_path) as connection:
            results = FluidStatements.check_file_cache(connection, full_path)

        return results

    def trim_file_cache(self, from_time):
        with sqlite3.connect(self.db_path) as connection:
            self._l.info("Trimming fluid file cache")
            FluidStatements.trim_file_entries(connection, from_time)

    def scan_breaks(self, dir_path):
        with sqlite3.connect(self.db_path) as connection:
            
            self._l.info(f"Scanning directory {dir_path} for breaks")
            if not os.path.isdir(dir_path):
                raise FileNotFoundError(f"Directory does not exist {dir_path}")
            dir_path = os.path.realpath(dir_path)
            file_list = MediaProcessor._rfind_media(dir_path)

            # Check the cache because we require the duration to prococess.
            file_paths = [os.path.realpath(file) for file in file_list]
            cached_files = {}
            for path in file_paths:
                cached = FluidStatements.check_file_cache(connection, path)
                if cached:
                    cached_files[path] = cached

            for file in file_list:
                rfp = os.path.realpath(file)
                if rfp in cached_files:
                    cached = cached_files[rfp]
                    if FluidStatements.get_break_points(connection, rfp):
                        self._l.info(f"Breaks already exists for {rfp}")
                    else:
                        breaks = MediaProcessor.black_detect(rfp, cached.duration)
                        FluidStatements.add_break_points(connection, rfp, breaks)
                else:
                    self._l.warning(f"{rfp} is not in catalog cache - not adding break points.")
            connection.commit()

    def get_breaks(self, fname):
        fname = os.path.realpath(fname)
        with sqlite3.connect(self.db_path) as connection:
            results = FluidStatements.get_break_points(connection, fname)
        return results


if __name__ == "__main__":
    logging.basicConfig(format="%(levelname)s:%(name)s:%(message)s", level=logging.INFO)
    builder = FluidBuilder()
    # builder.scan_file_cache("catalog/nbc_content/")
    # exists = builder.check_file_cache("FieldStation42/catalog/public_domain/bextra/post-black.mov")
    builder.scan_breaks("catalog/public_domain/feature/sub/a/")
