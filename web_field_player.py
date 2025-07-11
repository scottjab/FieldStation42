import argparse
import time
import datetime
import json
import signal
import logging
import asyncio
import threading
import os
from pathlib import Path
from typing import Optional, Dict, Any
import subprocess

from fastapi import FastAPI, HTTPException, WebSocket, WebSocketDisconnect
from fastapi.staticfiles import StaticFiles
from fastapi.responses import HTMLResponse, FileResponse, StreamingResponse
from fastapi.middleware.cors import CORSMiddleware
import uvicorn

from fs42.station_manager import StationManager
from fs42.timings import MIN_1, DAYS
from fs42.station_player import (
    PlayStatus,
    PlayerOutcome,
    check_channel_socket,
    update_status_socket,
)
from fs42.reception import ReceptionStatus
from fs42.liquid_manager import LiquidManager, PlayPoint, ScheduleNotFound, ScheduleQueryNotInBounds

logging.basicConfig(
    format="%(asctime)s %(levelname)s:%(name)s:%(message)s", level=logging.INFO
)

debounce_fragment = 0.1


class WebStationPlayer:
    """Web-based station player that streams video via HTTP instead of using MPV"""
    
    def __init__(self, station_config):
        self._l = logging.getLogger("WebFieldPlayer")
        self.station_config = station_config
        self.current_playing_file_path = None
        self.current_stream_url = None
        self.reception = ReceptionStatus()
        self.skip_reception_check = False
        self.scrambler = None
        self.current_process = None
        
    def shutdown(self):
        self.current_playing_file_path = None
        self.current_stream_url = None
        if self.current_process:
            self.current_process.kill()
            self.current_process = None
        
    def update_filters(self):
        # Web player doesn't apply video filters directly
        # They would need to be applied at the video source level
        pass
        
    def update_reception(self):
        if not self.reception.is_perfect():
            self.reception.improve()
            
    def play_file(self, file_path, file_duration=None, current_time=None, is_stream=False):
        try:
            if os.path.exists(file_path) or is_stream:
                self._l.debug(f"%%%Attempting to play {file_path}")
                self.current_playing_file_path = file_path
                
                # Kill any existing stream
                if self.current_process:
                    self.current_process.kill()
                    self.current_process = None
                
                # For web streaming, we need to serve the video file via HTTP
                if is_stream:
                    self.current_stream_url = file_path
                else:
                    # Convert local file path to web-accessible streaming URL
                    self.current_stream_url = f"/stream/{Path(file_path).name}"
                
                basename = os.path.basename(file_path)
                title, _ = os.path.splitext(basename)
                
                if self.station_config:
                    self._l.debug("Got station config, updating status socket")
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

                self._l.info(f"playing {file_path} via web stream at {self.current_stream_url}")
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
            
    def get_current_title(self):
        if self.current_playing_file_path:
            basename = os.path.basename(self.current_playing_file_path)
            title, _ = os.path.splitext(basename)
            return title
        return None
        
    def get_current_stream_url(self):
        return self.current_stream_url

    def show_guide(self, guide_config):
        """Show guide channel - adapted for web streaming instead of tkinter"""
        self._l.info("Starting guide channel for web player")
        
        # Set up guide video stream
        self.current_stream_url = "/guide_stream"
        
        # Run the guide loop like the original player
        keep_going = True
        while keep_going:
            time.sleep(0.05)
            response = check_channel_socket()
            if response:
                self._l.info("Guide channel received channel change command")
                return response

        return PlayerOutcome(PlayStatus.SUCCESS)

    def schedule_panic(self, network_name):
        self._l.critical("*********************Schedule Panic*********************")
        self._l.critical(f"Schedule not found for {network_name} - attempting to generate a one-day extention")
        from fs42.liquid_schedule import LiquidSchedule
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

                self._l.info(f"Seeking for: {total_skip}")

                if entry.duration:
                    self._l.info(f"Monitoring for: {entry.duration - initial_skip}")

                    # this is our main event loop
                    keep_waiting = True
                    stop_time = datetime.datetime.now() + datetime.timedelta(seconds=entry.duration - initial_skip)
                    while keep_waiting:
                        if not self.skip_reception_check:
                            self.update_reception()
                        else:
                            if self.scrambler:
                                # Web player doesn't apply filters directly
                                pass
                                
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


class WebFieldPlayer:
    """Main web field player that manages the web interface and station switching"""
    
    def __init__(self, host="0.0.0.0", port=9191):
        self.host = host
        self.port = port
        self.logger = logging.getLogger("WebFieldPlayer")
        self.app = FastAPI(title="FieldStation42 Web Player")
        self.manager = StationManager()
        self.reception = ReceptionStatus()
        self.player = None
        self.current_channel_index = 0
        self.websocket_connections = []
        self.running = False
        self.current_stream_process = None
        
        # Setup CORS for web interface
        self.app.add_middleware(
            CORSMiddleware,
            allow_origins=["*"],
            allow_credentials=True,
            allow_methods=["*"],
            allow_headers=["*"],
        )
        
        # Setup routes
        self.setup_routes()
        
    def setup_routes(self):
        @self.app.get("/")
        async def root():
            return HTMLResponse(self.get_html_interface())
            
        @self.app.get("/api/status")
        async def get_status():
            if self.player:
                status = {
                    "channel": self.manager.stations[se∞lf.current_channel_index]["channel_number"],
                    "name": self.manager.stations[self.current_channel_index]["network_name"],
                    "title": self.player.get_current_title() or "",
                    "stream_url": self.player.get_current_stream_url() or "",
                    "reception_quality": 1.0 - self.reception.chaos
                }
                self.logger.debug(f"Status: {status}")
                return status
            return {"channel": -1, "name": "", "title": "", "stream_url": "", "reception_quality": 0}
            
        @self.app.post("/api/channel/{channel_number}", response_model=dict)
        async def change_channel(channel_number: int):
            try:
                new_index = self.manager.index_from_channel(channel_number)
                if new_index is not None:
                    self.current_channel_index = new_index
                    await self.switch_channel()
                    return {"status": "ok", "channel": channel_number}
                else:
                    raise HTTPException(status_code=404, detail=f"Channel {channel_number} not found")
            except Exception as e:
                raise HTTPException(status_code=500, detail=str(e))
                
        @self.app.post("/api/channel/up", response_model=dict)
        async def channel_up():
            self.logger.info(f"Channel UP requested. Current: {self.current_channel_index}, Total stations: {len(self.manager.stations)}")
            self.current_channel_index = (self.current_channel_index + 1) % len(self.manager.stations)
            self.logger.info(f"New channel index: {self.current_channel_index}")
            # Don't call switch_channel here as it's async and we're in a sync context
            return {"status": "ok", "channel": self.manager.stations[self.current_channel_index]["channel_number"]}
            
        @self.app.post("/api/channel/down", response_model=dict)
        async def channel_down():
            self.logger.info(f"Channel DOWN requested. Current: {self.current_channel_index}, Total stations: {len(self.manager.stations)}")
            self.current_channel_index = (self.current_channel_index - 1) % len(self.manager.stations)
            self.logger.info(f"New channel index: {self.current_channel_index}")
            # Don't call switch_channel here as it's async and we're in a sync context
            return {"status": "ok", "channel": self.manager.stations[self.current_channel_index]["channel_number"]}
            
        @self.app.websocket("/ws")
        async def websocket_endpoint(websocket: WebSocket):
            await websocket.accept()
            self.websocket_connections.append(websocket)
            try:
                while True:
                    data = await websocket.receive_text()
                    # Handle websocket messages if needed
            except WebSocketDisconnect:
                self.websocket_connections.remove(websocket)
                
        @self.app.get("/stream/{filename}")
        async def serve_video(filename: str):
            # Serve video files from the content directories
            for station in self.manager.stations:
                if "content_dir" in station:
                    video_path = Path(station["content_dir"]) / filename
                    if video_path.exists():
                        return FileResponse(str(video_path))
            raise HTTPException(status_code=404, detail="Video not found")
            
        @self.app.get("/live")
        async def live_stream():
            """Main live stream endpoint - serves whatever is currently playing"""
            if not self.player or not self.player.current_playing_file_path:
                # Return a placeholder or error
                return HTMLResponse("No content currently playing", status_code=404)
            
            file_path = self.player.current_playing_file_path
            
            # Kill any existing stream process
            if self.current_stream_process:
                self.current_stream_process.kill()
                self.current_stream_process = None
            
            # Check if it's a local file or external stream
            if os.path.exists(file_path):
                # Local file - transcode with ffmpeg
                ffmpeg_cmd = [
                    "ffmpeg",
                    "-i", file_path,
                    "-f", "mp4",
                    "-vcodec", "libx264",
                    "-acodec", "aac",
                    "-movflags", "frag_keyframe+empty_moov+default_base_moof",
                    "-preset", "veryfast",
                    "-tune", "zerolatency",
                    "-b:v", "1M",
                    "-bufsize", "2M",
                    "-maxrate", "1M",
                    "-analyzeduration", "100M",
                    "-probesize", "100M",
                    "-y",
                    "-loglevel", "error",
                    "pipe:1"
                ]
                
                self.logger.info(f"Starting live stream for: {file_path}")
                self.logger.info(f"Running ffmpeg command: {' '.join(ffmpeg_cmd)}")
                
                self.current_stream_process = subprocess.Popen(
                    ffmpeg_cmd,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    bufsize=10**6
                )
                
                def iterfile():
                    try:
                        while True:
                            chunk = self.current_stream_process.stdout.read(1024*1024)
                            if not chunk:
                                break
                            yield chunk
                    finally:
                        if self.current_stream_process:
                            self.current_stream_process.stdout.close()
                            self.current_stream_process.stderr.close()
                            self.current_stream_process.kill()
                            self.current_stream_process = None
                
                return StreamingResponse(iterfile(), media_type="video/mp4")
            else:
                # External stream - proxy it
                # This would need to be implemented based on the stream type
                return HTMLResponse("External streams not yet supported", status_code=501)
            
        @self.app.get("/guide")
        async def serve_guide():
            """Serve guide channel content"""
            return HTMLResponse(self.get_guide_html())
            
        @self.app.get("/guide_stream")
        async def stream_guide():
            """Generate a dynamic guide video stream"""
            self.logger.info("Generating guide video stream")
            
            # Create a simple animated guide using ffmpeg
            # This creates a scrolling text guide with current time
            current_time = datetime.datetime.now().strftime("%H:%M:%S")
            ffmpeg_cmd = [
                "ffmpeg",
                "-f", "lavfi",
                "-i", f"color=black:size=1280x720:rate=1:duration=3600,drawtext=text='FieldStation42 Guide':fontcolor=green:fontsize=60:x=(w-text_w)/2:y=50:font=monospace,drawtext=text='Current Time: {current_time}':fontcolor=green:fontsize=40:x=50:y=150:font=monospace,drawtext=text='Use CH UP/DOWN to navigate':fontcolor=green:fontsize=30:x=50:y=200:font=monospace",
                "-f", "mp4",
                "-vcodec", "libx264",
                "-preset", "veryfast",
                "-tune", "zerolatency",
                "-b:v", "500k",
                "-bufsize", "1M",
                "-maxrate", "500k",
                "-g", "30",
                "-keyint_min", "30",
                "-sc_threshold", "0",
                "-movflags", "frag_keyframe+empty_moov+default_base_moof",
                "-y",
                "-loglevel", "error",
                "pipe:1"
            ]

            self.logger.info(f"Running guide ffmpeg command: {' '.join(ffmpeg_cmd)}")

            process = subprocess.Popen(
                ffmpeg_cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=10**6
            )

            def iterfile():
                try:
                    while True:
                        chunk = process.stdout.read(1024*1024)
                        if not chunk:
                            break
                        yield chunk
                finally:
                    process.stdout.close()
                    process.stderr.close()
                    process.kill()

            return StreamingResponse(iterfile(), media_type="video/mp4")
            
        @self.app.get("/static/guide_placeholder.png")
        async def serve_guide_placeholder():
            """Serve a placeholder image for guide channels"""
            # Create a simple placeholder image or return a default one
            return HTMLResponse("""
            <html>
            <head><title>FieldStation42 Guide</title></head>
            <body style="background: #000; color: #0f0; font-family: monospace; text-align: center; padding: 50px;">
                <h1>FieldStation42 Guide</h1>
                <p>Guide channel is currently active</p>
                <p>Use channel up/down to navigate</p>
            </body>
            </html>
            """)
            
    def get_html_interface(self):
        return """
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>FieldStation42 Web Player</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #000;
            color: #0f0;
            margin: 0;
            padding: 20px;
            overflow: hidden;
        }
        .container {
            display: flex;
            flex-direction: column;
            height: 100vh;
        }
        .video-container {
            flex: 1;
            position: relative;
            background-color: #111;
            border: 2px solid #0f0;
            border-radius: 10px;
            overflow: hidden;
        }
        video {
            width: 100%;
            height: 100%;
            object-fit: contain;
        }
        .controls {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 10px;
            background-color: #111;
            border: 2px solid #0f0;
            border-radius: 10px;
            margin-top: 10px;
        }
        .channel-info {
            display: flex;
            flex-direction: column;
            align-items: center;
        }
        .channel-number {
            font-size: 2em;
            font-weight: bold;
        }
        .channel-name {
            font-size: 1.2em;
        }
        .show-title {
            font-size: 1em;
            color: #0a0;
        }
        .control-buttons {
            display: flex;
            gap: 10px;
        }
        button {
            background-color: #000;
            color: #0f0;
            border: 2px solid #0f0;
            padding: 10px 20px;
            font-family: 'Courier New', monospace;
            font-size: 1em;
            cursor: pointer;
            border-radius: 5px;
            transition: all 0.3s;
        }
        button:hover {
            background-color: #0f0;
            color: #000;
        }
        .reception-indicator {
            width: 100px;
            height: 20px;
            background-color: #333;
            border: 1px solid #0f0;
            border-radius: 10px;
            overflow: hidden;
        }
        .reception-bar {
            height: 100%;
            background-color: #0f0;
            transition: width 0.3s;
        }
        .status {
            position: absolute;
            top: 10px;
            right: 10px;
            background-color: rgba(0, 0, 0, 0.8);
            padding: 10px;
            border-radius: 5px;
            font-size: 0.9em;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="video-container">
            <video id="videoPlayer" controls autoplay>
                Your browser does not support the video tag.
            </video>
            <div class="status" id="status">
                Loading...
            </div>
        </div>
        <div class="controls">
            <div class="channel-info">
                <div class="channel-number" id="channelNumber">--</div>
                <div class="channel-name" id="channelName">No Signal</div>
                <div class="show-title" id="showTitle"></div>
            </div>
            <div class="control-buttons">
                <button onclick="changeChannel('down')">CH DOWN</button>
                <button onclick="changeChannel('up')">CH UP</button>
            </div>
            <div class="reception-indicator">
                <div class="reception-bar" id="receptionBar" style="width: 0%"></div>
            </div>
        </div>
    </div>

    <script>
        let currentStreamUrl = '';
        
        async function updateStatus() {
            try {
                const response = await fetch('/api/status');
                const status = await response.json();
                
                document.getElementById('channelNumber').textContent = status.channel || '--';
                document.getElementById('channelName').textContent = status.name || 'No Signal';
                document.getElementById('showTitle').textContent = status.title || '';
                document.getElementById('receptionBar').style.width = (status.reception_quality * 100) + '%';
                
                // Always use the live stream endpoint
                const video = document.getElementById('videoPlayer');
                if (video.src !== '/live') {
                    video.src = '/live';
                    video.load();
                    video.play().catch(e => console.log('Auto-play prevented:', e));
                }
                
                document.getElementById('status').textContent = 
                    `Quality: ${Math.round(status.reception_quality * 100)}%`;
                    
            } catch (error) {
                console.error('Error updating status:', error);
                document.getElementById('status').textContent = 'Connection Error';
            }
        }
        
        async function changeChannel(direction) {
            try {
                const response = await fetch(`/api/channel/${direction}`, { method: 'POST' });
                const result = await response.json();
                console.log('Channel changed:', result);
            } catch (error) {
                console.error('Error changing channel:', error);
            }
        }
        
        // Update status every second
        setInterval(updateStatus, 1000);
        
        // Initial status update
        updateStatus();
        
        // Keyboard controls
        document.addEventListener('keydown', (event) => {
            switch(event.key) {
                case 'ArrowUp':
                    changeChannel('up');
                    break;
                case 'ArrowDown':
                    changeChannel('down');
                    break;
                case ' ':
                    // Toggle play/pause
                    const video = document.getElementById('videoPlayer');
                    if (video.paused) {
                        video.play();
                    } else {
                        video.pause();
                    }
                    break;
            }
        });
    </script>
</body>
</html>
        """
        
    def get_guide_html(self):
        """Generate HTML for the guide channel"""
        return """
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>FieldStation42 Guide</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #000;
            color: #0f0;
            margin: 0;
            padding: 20px;
            overflow: hidden;
        }
        .guide-container {
            display: flex;
            flex-direction: column;
            height: 100vh;
        }
        .header {
            text-align: center;
            padding: 20px;
            border-bottom: 2px solid #0f0;
        }
        .content {
            flex: 1;
            display: flex;
            justify-content: center;
            align-items: center;
            text-align: center;
        }
        .guide-text {
            font-size: 1.5em;
            line-height: 1.6;
        }
        .footer {
            text-align: center;
            padding: 20px;
            border-top: 2px solid #0f0;
        }
    </style>
</head>
<body>
    <div class="guide-container">
        <div class="header">
            <h1>FieldStation42 Guide</h1>
        </div>
        <div class="content">
            <div class="guide-text">
                <p>Welcome to the FieldStation42 Guide Channel</p>
                <p>Use channel up/down to navigate between stations</p>
                <p>Guide functionality coming soon...</p>
            </div>
        </div>
        <div class="footer">
            <p>FieldStation42 - Retro TV Experience</p>
        </div>
    </div>
</body>
</html>
        """
        
    async def switch_channel(self):
        """Switch to the current channel index"""
        if not self.manager.stations:
            self.logger.error("No stations configured")
            return
            
        channel_conf = self.manager.stations[self.current_channel_index]
        self.player = WebStationPlayer(channel_conf)
        
        # Notify websocket clients
        status = {
            "channel": channel_conf["channel_number"],
            "name": channel_conf["network_name"],
            "title": "",
            "stream_url": "",
            "reception_quality": 1.0 - self.reception.chaos
        }
        
        for websocket in self.websocket_connections:
            try:
                await websocket.send_text(json.dumps(status))
            except:
                pass
                
    async def start_server(self):
        """Start the web server"""
        config = uvicorn.Config(
            self.app,
            host=self.host,
            port=self.port,
            log_level="info"
        )
        server = uvicorn.Server(config)
        await server.serve()
        
    def run_server(self):
        """Run the server in a separate thread"""
        asyncio.run(self.start_server())


def main_loop(transition_fn, host="0.0.0.0", port=9191):
    manager = StationManager()
    reception = ReceptionStatus()
    logger = logging.getLogger("MainLoop")
    logger.info("Starting web field player main loop")

    channel_socket = StationManager().server_conf["channel_socket"]

    # go ahead and clear the channel socket (or create if it doesn't exist)
    with open(channel_socket, "w"):
        pass

    channel_index = 0
    if not len(manager.stations):
        logger.error(
            "Could not find any station runtimes - do you have your channels configured?"
        )
        logger.error(
            "Check to make sure you have valid json configurations in the confs dir"
        )
        logger.error(
            "The confs/examples folder contains working examples that you can build off of - just move one into confs/"
        )
        return

    # Create web player
    web_player = WebFieldPlayer(host=host, port=port)
    web_player.player = WebStationPlayer(manager.stations[channel_index])
    player = web_player.player  # Use the same player instance
    reception.degrade()
    player.update_filters()

    def sigint_handler(sig, frame):
        logger.critical("Received sig-int signal, attempting to exit gracefully...")
        player.shutdown()
        web_player.running = False
        update_status_socket("stopped", "", -1)
        logger.info("Shutdown completed as expected - exiting application")
        exit(0)

    signal.signal(signal.SIGINT, sigint_handler)

    channel_conf = manager.stations[channel_index]

    # Start web server in background thread
    server_thread = threading.Thread(target=web_player.run_server, daemon=True)
    server_thread.start()
    
    logger.info(f"Web player started at http://{web_player.host}:{web_player.port}")
    logger.info("Open your browser to view the FieldStation42 web interface")
    
    # Debug: Show configured stations
    logger.info(f"Configured stations: {len(manager.stations)}")
    for i, station in enumerate(manager.stations):
        logger.info(f"  {i}: {station['network_name']} (Channel {station['channel_number']}, Type: {station['network_type']})")

    # this is actually the main loop
    outcome = None
    skip_play = False
    stuck_timer = 0

    while True:
        logger.info(f"Playing station: {channel_conf['network_name']}")

        if channel_conf["network_type"] == "guide" and not skip_play:
            logger.info("Starting the guide channel")
            outcome = player.show_guide(channel_conf)
        elif not skip_play:
            now = datetime.datetime.now()

            week_day = DAYS[now.weekday()]
            hour = now.hour
            skip = now.minute * MIN_1 + now.second

            logger.info(
                f"Starting station {channel_conf['network_name']} at: {week_day} {hour} skipping={skip} "
            )

            # Use the same scheduling logic as the original player
            outcome = player.play_slot(
                channel_conf["network_name"], datetime.datetime.now()
            )

        logger.debug(f"Got player outcome:{outcome.status}")

        # reset skip
        skip_play = False
        
        # Check if web interface requested a channel change
        if web_player.current_channel_index != channel_index:
            logger.info(f"Web interface requested channel change from {channel_index} to {web_player.current_channel_index}")
            channel_index = web_player.current_channel_index
            channel_conf = manager.stations[channel_index]
            web_player.player = WebStationPlayer(manager.stations[channel_index])
            player = web_player.player
            transition_fn(player, reception)
            continue

        if outcome.status == PlayStatus.CHANNEL_CHANGE:
            stuck_timer = 0
            tune_up = True
            # get the json payload
            if outcome.payload:
                try:
                    as_obj = json.loads(outcome.payload)
                    if "command" in as_obj:
                        if as_obj["command"] == "direct":
                            tune_up = False
                            if "channel" in as_obj:
                                logger.debug(
                                    f"Got direct tune command for channel {as_obj['channel']}"
                                )
                                new_index = manager.index_from_channel(
                                    as_obj["channel"]
                                )
                                if new_index is None:
                                    logger.warning(
                                        f"Got direct tune command but could not find station with channel {as_obj['channel']}"
                                    )
                                else:
                                    channel_index = new_index
                            else:
                                logger.critical(
                                    "Got direct tune command, but no channel specified"
                                )
                        elif as_obj["command"] == "up":
                            tune_up = True
                            logger.debug("Got channel up command")
                        elif as_obj["command"] == "down":
                            tune_up = False
                            logger.debug("Got channel down command")
                            channel_index -= 1
                            if channel_index < 0:
                                channel_index = len(manager.stations) - 1

                except Exception as e:
                    logger.exception(e)
                    logger.warning(
                        "Got payload on channel change, but JSON convert failed"
                    )

            if tune_up:
                logger.info("Starting channel change")
                channel_index += 1
                if channel_index >= len(manager.stations):
                    channel_index = 0

            channel_conf = manager.stations[channel_index]
            player.station_config = channel_conf

            # Update web player
            web_player.current_channel_index = channel_index
            web_player.player = WebStationPlayer(manager.stations[channel_index])
            player = web_player.player  # Update the player reference

            # long_change_effect(player, reception)
            transition_fn(player, reception)

        elif outcome.status == PlayStatus.FAILED:
            stuck_timer += 1

            # only put it up once after 2 seconds of being stuck
            if stuck_timer >= 2 and "standby_image" in channel_conf:
                player.play_file(channel_conf["standby_image"])
            current_title_on_stuck = player.get_current_title()
            update_status_socket(
                "stuck",
                channel_conf["network_name"],
                channel_conf["channel_number"],
                current_title_on_stuck,
            )

            time.sleep(1)
            logger.critical(
                "Player failed to start - resting for 1 second and trying again"
            )

            # check for channel change so it doesn't stay stuck on a broken channel
            new_outcome = check_channel_socket()
            if new_outcome is not None:
                outcome = new_outcome
                # set skip play so outcome isn't overwritten
                # and the channel change can be processed next loop
                skip_play = True
        elif outcome.status == PlayStatus.SUCCESS:
            stuck_timer = 0
        else:
            stuck_timer = 0


def none_change_effect(player, reception):
    pass


def short_change_effect(player, reception):
    prev = reception.improve_amount
    reception.improve_amount = 0

    while not reception.is_degraded():
        reception.degrade(0.2)
        player.update_filters()
        time.sleep(debounce_fragment)

    reception.improve_amount = prev


def long_change_effect(player, reception):
    # add noise to current channel
    while not reception.is_degraded():
        reception.degrade()
        player.update_filters()
        time.sleep(debounce_fragment)

    # reception.improve(1)
    player.play_file("runtime/static.mp4")
    while not reception.is_perfect():
        reception.improve()
        player.update_filters()
        time.sleep(debounce_fragment)
    # time.sleep(1)
    while not reception.is_degraded():
        reception.degrade()
        player.update_filters()
        time.sleep(debounce_fragment)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="FieldStation42 Web Player")
    parser.add_argument(
        "-t",
        "--transition",
        choices=["long", "short", "none"],
        help="Transition effect to use on channel change",
    )
    parser.add_argument(
        "-l", "--logfile", help="Set logging to use output file - will append each run"
    )
    parser.add_argument(
        "-v",
        "--verbose",
        action="store_true",
        help="Set logging verbosity level to very chatty",
    )
    parser.add_argument(
        "--host",
        default="0.0.0.0",
        help="Host to bind the web server to (default: 0.0.0.0)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=9191,
        help="Port to bind the web server to (default: 9191)",
    )
    args = parser.parse_args()

    if args.verbose:
        logging.getLogger().setLevel(logging.DEBUG)

    if args.logfile:
        formatter = logging.Formatter("%(asctime)s:%(levelname)s:%(name)s:%(message)s")
        fh = logging.FileHandler(args.logfile)
        fh.setFormatter(formatter)

        logging.getLogger().addHandler(fh)

    trans_fn = short_change_effect

    if args.transition:
        if args.transition == "long":
            trans_fn = long_change_effect
        elif args.transition == "none":
            trans_fn = none_change_effect
        # else keep short change as default

    main_loop(trans_fn, host=args.host, port=args.port) 