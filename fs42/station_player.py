from enum import Enum
import logging

import multiprocessing
import time
import datetime
import json
import os

from python_mpv_jsonipc import MPV

from fs42.guide_tk import guide_channel_runner, GuideCommands
from fs42.reception import ReceptionStatus
from fs42.liquid_manager import LiquidManager, PlayPoint, ScheduleNotFound, ScheduleQueryNotInBounds

from fs42.liquid_schedule import LiquidSchedule
from fs42.station_manager import StationManager

logging.basicConfig(format="%(asctime)s %(levelname)s:%(name)s:%(message)s", level=logging.INFO)


def check_channel_socket():
    channel_socket = StationManager().server_conf["channel_socket"]
    with open(channel_socket, "r") as r_sock:
        contents = r_sock.read()
    if len(contents):
        with open(channel_socket, "w"):
            pass
        return PlayerOutcome(PlayStatus.CHANNEL_CHANGE, contents)
    return None


def update_status_socket(
    status, network_name, channel, title=None, timestamp="%Y-%m-%dT%H:%M:%S", duration=None, file_path=None
):
    status_obj = {
        "status": status,
        "network_name": network_name,
        "channel_number": channel,
        "timestamp": datetime.datetime.now().strftime(timestamp),
    }
    if title is not None:
        status_obj["title"] = title
    if duration is not None:
        status_obj["duration"] = duration
    if file_path is not None:
        status_obj["file_path"] = file_path
    status_socket = StationManager().server_conf["status_socket"]
    as_str = json.dumps(status_obj)
    with open(status_socket, "w") as fp:
        fp.write(as_str)


class PlayStatus(Enum):
    FAILED = 1
    EXITED = 2
    SUCCESS = 3
    CHANNEL_CHANGE = 4


class PlayerOutcome:
    def __init__(self, status=PlayStatus.SUCCESS, payload=None):
        self.status = status
        self.payload = payload


class StationPlayer:
    def __init__(self, station_config, mpv=None):
        self._l = logging.getLogger("FieldPlayer")

        start_it = True

        if "start_mpv" in StationManager().server_conf:
            start_it = StationManager().server_conf["start_mpv"]

        if not mpv:
            self._l.info("Starting MPV instance")
            # command on client: mpv --input-ipc-server=/tmp/mpvsocket --idle --force-window
            self.mpv = MPV(
                start_mpv=start_it,
                ipc_socket="/tmp/mpvsocket",
                input_default_bindings=False,
                fs=True,
                idle=True,
                force_window=True,
                script_opts="osc-idlescreen=no",
            )
        self.station_config = station_config
        # self.playlist = self.read_json(runtime_filepath)
        self.index = 0
        self.reception = ReceptionStatus()
        self.current_playing_file_path = None

    def show_text(self, text, duration=4):
        self.mpv.command("show-text", text, duration)

    def shutdown(self):
        self.current_playing_file_path = None
        self.mpv.terminate()

    def update_filters(self):
        self.mpv.vf = self.reception.filter()

    def update_reception(self):
        if not self.reception.is_perfect():
            self.reception.improve()
            # did that get us below threshhold?
            if self.reception.is_perfect():
                self.mpv.vf = ""
            else:
                self.mpv.vf = self.reception.filter()

    def play_file(self, file_path, file_duration=None, current_time=None, is_stream=False):
        try:
            if os.path.exists(file_path) or is_stream:
                self._l.info(f"%%%Attempting to play {file_path}")
                self.current_playing_file_path = file_path
                basename = os.path.basename(file_path)
                title, _ = os.path.splitext(basename)
                if self.station_config:
                    self._l.info("%%%Got station config, updating status socket")
                    if "date_time_format" in StationManager().server_conf:
                        ts_format = StationManager().server_conf["date_time_format"]
                    else:
                        ts_format = "%Y-%m-%dT%H:%M:%S"
                    duration = (
                        f"{str(datetime.timedelta(seconds=int(current_time)))}/{str(datetime.timedelta(seconds=int(file_duration)))}"
                        if file_duration
                        else "n/a"
                    )
                    update_status_socket(
                        "playing",
                        self.station_config["network_name"],
                        self.station_config["channel_number"],
                        title,
                        timestamp=ts_format,
                        duration=duration,
                        file_path=file_path,
                    )
                else:
                    self._l.warning(
                        "station_config not available in play_file, cannot update status socket with title."
                    )

                if "panscan" in self.station_config:
                    self.mpv.panscan = self.station_config["panscan"]
                self._l.info(f"playing {file_path}")
                self.mpv.play(file_path)
                self.mpv.wait_for_property("duration")
                self._l.info("## Set duration, returning true")

                return True
            else:
                self._l.error(
                    f"Trying to play file {file_path} but it doesn't exist - check your configuration and try again."
                )
                return False
        except Exception as e:
            self._l.exception(e)
            self._l.error(
                f"Encountered unknown error attempting to play {file_path} - please check your configurations."
            )
            return False

    def play_image(self, duration):
        pass

    def show_guide(self, guide_config):
        # create the pipe to communicate with the guide channel
        queue = multiprocessing.Queue()
        guide_process = multiprocessing.Process(
            target=guide_channel_runner,
            args=(
                guide_config,
                queue,
            ),
        )
        guide_process.start()

        if "play_sound" in guide_config and guide_config["play_sound"]:
            # make sure it actually exists
            playing = self.play_file(guide_config["sound_to_play"])
            if not playing:
                self.mpv.stop()
                self.current_playing_file_path = None
        else:
            self.mpv.stop()
            self.current_playing_file_path = None
        keep_going = True
        while keep_going:
            time.sleep(0.05)
            response = check_channel_socket()
            if response:
                self._l.info("Sending the guide channel shutdown command")
                queue.put(GuideCommands.hide_window)
                guide_process.join()
                return response

        return PlayerOutcome(PlayStatus.SUCCESS)

    def schedule_panic(self, network_name):
        self._l.critical("*********************Schedule Panic*********************")
        self._l.critical(f"Schedule not found for {network_name} - attempting to generate a one-day extention")
        schedule = LiquidSchedule(StationManager().station_by_name(network_name))
        schedule.add_days(1)
        self._l.warning(f"Schedule extended for {network_name} - reloading schedules now")
        LiquidManager().reload_schedules()

    def play_slot(self, network_name, when):
        liquid = LiquidManager()
        try:
            play_point = liquid.get_play_point(network_name, when)
        except (ScheduleNotFound, ScheduleQueryNotInBounds):
            self.schedule_panic(network_name)
            self._l.warning(f"Schedules reloaded - retrying play for: {network_name}")
            # fail so we can return and try again
            return PlayerOutcome(PlayStatus.FAILED)

        if play_point is None:
            self.current_playing_file_path = None
            return PlayerOutcome(PlayStatus.FAILED)
        return self._play_from_point(play_point)

    # returns true if play is interrupted
    def _play_from_point(self, play_point: PlayPoint):
        if len(play_point.plan):
            initial_skip = play_point.offset

            # iterate over the slice from index to end
            for entry in play_point.plan[play_point.index :]:
                self._l.info(f"Playing entry {entry}")
                self._l.info(f"Initial Skip: {initial_skip}")
                total_skip = entry.skip + initial_skip

                is_stream = False

                if hasattr(entry, "is_stream"):
                    is_stream = entry.is_stream

                self.play_file(entry.path, file_duration=entry.duration, current_time=total_skip, is_stream=is_stream)

                try:
                    self.mpv.seek(total_skip)
                except Exception:
                    self._l.error(f"Failed seeking on {entry.path}")
                    return PlayerOutcome(PlayStatus.FAILED)

                self._l.info(f"Seeking for: {total_skip}")

                if entry.duration:
                    self._l.info(f"Monitoring for: {entry.duration - initial_skip}")

                    # this is our main event loop
                    keep_waiting = True
                    stop_time = datetime.datetime.now() + datetime.timedelta(seconds=entry.duration - initial_skip)
                    while keep_waiting:
                        self.update_reception()
                        now = datetime.datetime.now()

                        if now >= stop_time:
                            keep_waiting = False
                        else:
                            # debounce time
                            time.sleep(0.05)
                            response = check_channel_socket()
                            if response:
                                return response
                else:
                    return PlayerOutcome(PlayStatus.FAILED)

                initial_skip = 0

            self._l.info("Done playing block")
            return PlayerOutcome(PlayStatus.SUCCESS)
        else:
            self.current_playing_file_path = None
            return PlayerOutcome(PlayStatus.FAILED, "Failure getting index...")

    def get_current_title(self):
        if self.current_playing_file_path:
            basename = os.path.basename(self.current_playing_file_path)
            title, _ = os.path.splitext(basename)
            return title
        return None
