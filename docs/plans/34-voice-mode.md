# 34 — Local voice mode

**Status: PLANNED.** Provider-free local English transcription and speech
output for Agent Kate using Whisper and Piper.

## Goals

- Provide local English speech-to-text with Whisper.
- Provide local English text-to-speech with Piper's Amy voice.
- Allow input and output devices to be selected independently of the operating
  system defaults.
- Support push-to-talk dictation.
- Support conversational/open-mic mode.
- Use echo cancellation so Agent Kate's voice does not trigger conversational
  turn completion or interruption.
- Keep real-time audio processing out of the main UI thread.

The selected Piper voice is
[en_US-amy-medium](https://huggingface.co/rhasspy/piper-voices/tree/main/en/en_US/amy/medium).
The main speech-recognition engine will be
[whisper.cpp](https://github.com/ggml-org/whisper.cpp), initially using the
base.en model.

## Architecture

Add a native Qt/C++ akvoice helper process responsible for:

- Microphone capture
- Speaker playback
- Audio-device enumeration
- Whisper inference
- Piper invocation or embedding
- Voice activity detection
- Echo cancellation
- Conversation state

akcore will supervise the helper, while the UI communicates with it through
the existing local JSON-RPC bus. This keeps audio processing out of the UI
thread and avoids sending real-time PCM data through the Go core.

## 1. Independent audio-device routing

The application will enumerate PipeWire/Qt audio devices and persist separate
selections:

~~~
inputDeviceId
outputDeviceId
aecEnabled
~~~

The selected input and output devices will be opened explicitly. Agent Kate
must not change the operating system's default microphone or speaker.

The settings UI should provide:

- Microphone/input selector
- Speaker/output selector
- Refresh devices
- Input-level meter
- Microphone test
- Amy-voice test
- Echo-cancellation toggle
- Device-availability state

[Qt Multimedia](https://doc.qt.io/qt-6/qtmultimedia-index.html) provides the
application-level capture and playback APIs. [PipeWire's echo-cancel
module](https://docs.pipewire.org/page_pulse_module_echo_cancel.html) provides
explicit source and sink masters for the AEC path.

If a selected device disappears, Agent Kate should retain the preference, use a
temporary session fallback, and clearly report the fallback to the user.

## 2. Model management

Store models under the application data directory, for example:

~~~
~/.local/share/agentkate/speech/models/
├── whisper/
│   └── ggml-base.en.bin
└── piper/
    ├── en_US-amy-medium.onnx
    └── en_US-amy-medium.onnx.json
~~~

The model manager will:

- Detect whether required models are installed.
- Offer an explicit **Download models** action.
- Verify file size and checksums where available.
- Report missing or corrupt models.
- Never download models implicitly during dictation.
- Run fully offline after installation.

## 3. Push-to-talk mode

Initial behavior:

1. The user presses the configured shortcut or toolbar button.
2. Audio capture starts.
3. Whisper receives the microphone stream.
4. Partial text appears in the composer.
5. The user releases the key.
6. The final transcript is inserted into the composer.
7. Agent Kate optionally sends it, based on a setting.

The default behavior should be insert-only so accidental transcription never
submits a message without user confirmation.

Actions should be registered with the existing KDE action collection and global
shortcut system described in [plan 27](27-kde-presence.md):

- Toggle voice dictation
- Push-to-talk
- Start/stop conversational mode
- Interrupt assistant speech
- Test microphone
- Test voice

## 4. Conversational mode

Use an explicit state machine rather than reacting to microphone volume alone:

~~~
Idle → Listening → Thinking → Speaking → Listening
~~~

Additional transitions:

~~~
Speaking + confirmed user speech → Interrupting → Listening
Listening + sustained silence  → Thinking
Thinking + new user speech     → Listening
Any state + stop               → Idle
~~~

Turn detection should combine:

- Voice activity detection
- Whisper partial transcription
- Speech confidence
- Minimum speech duration
- Silence duration
- Current assistant-playback state
- Echo-cancelled microphone audio

## 5. Intelligent turn-taking and echo cancellation

The assistant must not stop merely because the microphone level rises.

A user interruption should require:

- Echo-cancelled speech energy above threshold
- Speech persisting for a minimum duration
- VAD classification as human speech
- Either a valid Whisper partial or strong speech confidence

While Piper is speaking:

- Process the microphone signal through AEC.
- Supply the actual speaker playback stream as the AEC reference.
- Stop assistant audio quickly when genuine user speech is detected.
- Preserve the current assistant response in the transcript.
- Interrupt playback without discarding already-generated text.
- Apply a short post-playback guard so the final audio tail cannot trigger a
  new turn.

The system should distinguish:

~~~
Assistant voice detected in microphone → ignore
User voice detected over assistant voice → interrupt
Room noise or keyboard noise → ignore
~~~

The echo canceller must receive the same output stream Agent Kate is actually
playing, not merely the generated text.

## 6. Response cutoff policy

Conversational responses should be streamed to Piper in sentence-sized chunks
rather than waiting for the complete response.

Recommended behavior:

- Queue one sentence or short paragraph at a time.
- Play only the current chunk.
- Stop playback immediately after confirmed user interruption.
- Do not start a new response while the user is speaking.
- Cancel queued audio when a new user turn is accepted.
- Enforce a configurable maximum response duration.
- Provide a dedicated **Interrupt assistant** action.

This provides natural barge-in behavior without losing the text response from
the transcript.

## 7. JSON-RPC surface

Likely new methods:

~~~
speech.devices
speech.configure
speech.startPushToTalk
speech.stopPushToTalk
speech.startConversation
speech.stopConversation
speech.interrupt
speech.speak
speech.testInput
speech.testOutput
~~~

Likely notifications:

~~~
speech.deviceState
speech.stateChanged
speech.transcriptPartial
speech.transcriptFinal
speech.level
speech.error
speech.playbackStarted
speech.playbackFinished
speech.interrupted
~~~

Audio bytes must not travel through JSON-RPC. The speech helper owns the
real-time audio streams; JSON-RPC carries control commands and transcript
events only.

## 8. Testing milestones

### Milestone 1 — Local transcription

- Download base.en.
- Capture from a selected microphone.
- Produce final English transcripts.
- Verify that transcription works without network access.
- Verify that releasing push-to-talk reliably finalizes the utterance.

### Milestone 2 — Amy playback

- Install Piper and the Amy medium voice.
- Generate and play local speech.
- Verify that the selected output device is respected.
- Verify that changing the output does not change the operating system default.

### Milestone 3 — Independent routing

Test combinations such as:

- Laptop microphone → USB headset
- USB microphone → HDMI output
- Webcam microphone → laptop speakers
- Bluetooth microphone → wired headphones

Confirm that input and output choices remain independent.

### Milestone 4 — Conversational mode

- Automatic speech start and stop
- Silence-based turn completion
- Assistant response playback
- User interruption
- Queued-response cancellation
- Recovery after device disconnect

### Milestone 5 — Echo cancellation

Test:

- Amy speaking through laptop speakers while open mic is active
- User speaking during Amy playback
- User speaking immediately after Amy finishes
- Headphones, speakers, and Bluetooth devices
- Background noise and keyboard noise

Acceptance criterion: Amy's playback must not independently end the user's turn
or trigger a false interruption.

## 9. Settings and safety

Persist:

~~~
voiceMode
inputDeviceId
outputDeviceId
aecEnabled
whisperModel
piperVoice
autoSubmitDictation
conversationSilenceMs
bargeInEnabled
maximumResponseSeconds
~~~

Defaults:

- Push-to-talk enabled
- Automatic submission disabled
- Conversational mode disabled
- Echo cancellation enabled when speakers are selected
- Echo cancellation optional for headphones
- No audio recording saved unless explicitly requested
- No provider credentials or cloud endpoints involved
