#!/bin/sh
# Generate local Chinese PCM test samples with macOS system speech.
set -eu
output=${1:?usage: car-audio-samples.sh OUTPUT_DIRECTORY}
mkdir -p "$output"
for name in close-window rear-left chat; do
	case "$name" in
		close-window) text='把车窗关一下' ;;
		rear-left) text='左后窗' ;;
		chat) text='你好' ;;
	esac
	say -v Tingting --data-format=LEI16@16000 -o "$output/$name.wav" "$text"
	python3 - "$output/$name.wav" "$output/$name.pcm" <<'PY'
import sys
import wave

with wave.open(sys.argv[1], "rb") as sample:
    if (sample.getnchannels(), sample.getsampwidth(), sample.getframerate()) != (1, 2, 16000):
        raise SystemExit("unexpected audio format")
    with open(sys.argv[2], "wb") as pcm:
        pcm.write(sample.readframes(sample.getnframes()))
PY
done
