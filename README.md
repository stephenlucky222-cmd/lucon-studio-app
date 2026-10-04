# Lucon Studio for Mac and Windows

The Lucon Studio app: the Studio from events.luconhouse.com in its own window, with its own engine,
Lucon Link built in (streaming, professional recording, PTZ cameras) and free offline captions with Whisper.

Downloads: see **Releases** on the right.

* Mac with an Apple chip: `Lucon-Studio-Mac-Apple.dmg`
* Mac with an Intel processor: `Lucon-Studio-Mac-Intel.dmg`
* Windows 10 or 11: `Lucon-Studio-Windows-Setup.exe`

## How it is built

GitHub builds and tests everything (`.github/workflows/build.yml`) on every change:
Lucon Link (`link/`, Go), Whisper (whisper.cpp) and its models, then the app (Electron).
A tag like `v1.0.1` publishes a new version; the apps see it and offer the update.

Third-party parts: Electron (MIT), whisper.cpp (MIT), Whisper models by OpenAI (MIT).
