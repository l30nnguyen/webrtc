# Stable decisions

- Recording control uses the current viewer data channel: type recording, actions list/play/stop, requestId, and relative path for play. Replies are list/playing/live/ended/error.
- RecordingController ignores stale connections and superseded media requests. Recordings switch only the requesting viewer; stop and EOF return to live.
- Raw camera commands use the existing configurable cmdServer at 127.0.0.1:9191.
