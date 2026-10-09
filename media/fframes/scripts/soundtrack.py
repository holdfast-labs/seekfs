"""Original 120 BPM seekfs launch cue. Standard library, no sampled music."""
import math
import random
import wave
from array import array
from pathlib import Path

RATE = 44100
SECONDS = 30
CUTS = (5, 11, 17, 23)
random.seed(1110)
notes = ((220, 261.63, 329.63, 440), (174.61, 220, 261.63, 349.23),
         (130.81, 164.81, 196, 261.63), (196, 246.94, 293.66, 392))
samples = array('h')
for n in range(RATE * SECONDS):
    t = n / RATE
    quarter = t % 0.5
    eighth = t % 0.25
    chord = notes[int(t / 4) % 4]
    lead = chord[int(t * 4) % 4] * 2
    envelope = min(1, eighth / 0.012) * math.exp(-eighth * 12)
    signal = 0.12 * envelope * (math.sin(2 * math.pi * lead * t)
        + 0.25 * math.sin(4 * math.pi * lead * t))
    signal += 0.17 * math.exp(-quarter * 20) * math.sin(
        2 * math.pi * (52 * quarter + 3.4 * (1 - math.exp(-quarter * 35))))
    signal += 0.09 * math.sin(2 * math.pi * chord[0] / 2 * t) * (0.7 + 0.3 * math.sin(math.pi * t))
    signal += 0.027 * random.uniform(-1, 1) * math.exp(-eighth * 85)
    for cut in CUTS:
        d = t - cut
        if -0.30 < d < 0.30:
            signal += 0.07 * random.uniform(-1, 1) * (1 - abs(d) / 0.30)
    signal *= min(1, t / 0.12, (SECONDS - t) / 1.5)
    samples.append(int(max(-1, min(1, signal)) * 32767))
output = Path(__file__).resolve().parents[1] / 'media' / 'launch-raw.wav'
with wave.open(str(output), 'wb') as audio:
    audio.setnchannels(1)
    audio.setsampwidth(2)
    audio.setframerate(RATE)
    audio.writeframes(samples.tobytes())
print(output)
