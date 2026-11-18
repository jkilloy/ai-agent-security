package admin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Encoding names accepted in call variables, matching the detector service.
var encodings = map[string]string{
	"pcm16": "pcm16", "linear16": "pcm16", "s16le": "pcm16", "l16": "pcm16",
	"mulaw": "mulaw", "ulaw": "mulaw", "pcmu": "mulaw", "g711u": "mulaw",
	"alaw": "alaw", "pcma": "alaw", "g711a": "alaw",
	"f32le": "f32le", "float32": "f32le",
	"pcm8": "u8", "u8": "u8",
}

var widths = map[string]int64{"pcm16": 2, "mulaw": 1, "alaw": 1, "f32le": 4, "u8": 1}

// AudioFormat describes a call's raw audio, from its variables.
type AudioFormat struct {
	Encoding   string
	SampleRate int
	Channels   int
}

// FormatFromVariables reads encoding, sample_rate and channels.
func FormatFromVariables(v map[string]any) (AudioFormat, error) {
	f := AudioFormat{Channels: 1}
	enc, _ := v["encoding"].(string)
	f.Encoding = encodings[strings.ToLower(strings.TrimSpace(enc))]
	if f.Encoding == "" {
		return f, fmt.Errorf("unknown audio encoding %q", enc)
	}
	sr, _ := v["sample_rate"].(float64)
	if sr < 1000 || sr > 384000 || sr != math.Trunc(sr) {
		return f, errors.New("missing or invalid sample_rate")
	}
	f.SampleRate = int(sr)
	if ch, ok := v["channels"].(float64); ok {
		if ch < 1 || ch > 8 || ch != math.Trunc(ch) {
			return f, errors.New("invalid channels")
		}
		f.Channels = int(ch)
	}
	return f, nil
}

var mulawTable, alawTable [256]int16

func init() {
	for i := range 256 {
		u := ^uint8(i)
		exp := (u >> 4) & 7
		mag := ((int32(u&0x0F) << 3) + 0x84) << exp
		mag -= 0x84
		if u&0x80 != 0 {
			mag = -mag
		}
		mulawTable[i] = int16(mag)

		a := uint8(i) ^ 0x55
		exp = (a >> 4) & 7
		m := int32(a & 0x0F)
		var s int32
		if exp == 0 {
			s = (m << 4) + 8
		} else {
			s = ((m << 4) + 0x108) << (exp - 1)
		}
		if a&0x80 == 0 {
			s = -s
		}
		alawTable[i] = int16(s)
	}
}

const wavHeaderLen = 44

// wavReader presents raw call audio as a 16-bit PCM WAV file, converting on
// the fly. It is seekable, so http.ServeContent can serve byte ranges and
// the browser can seek without the whole file being converted.
type wavReader struct {
	src     io.ReaderAt
	format  AudioFormat
	inWidth int64
	header  []byte
	size    int64
	off     int64
}

func newWAVReader(src io.ReaderAt, srcSize int64, f AudioFormat) *wavReader {
	in := widths[f.Encoding]
	frame := in * int64(f.Channels)
	samples := (srcSize / frame) * int64(f.Channels)
	dataLen := samples * 2

	h := make([]byte, wavHeaderLen)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.SampleRate*f.Channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(f.Channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))

	return &wavReader{src: src, format: f, inWidth: in, header: h, size: wavHeaderLen + dataLen}
}

func (r *wavReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.off = offset
	return offset, nil
}

func (r *wavReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	n := 0
	if r.off < wavHeaderLen {
		n = copy(p, r.header[r.off:])
		r.off += int64(n)
		p = p[n:]
	}
	if len(p) == 0 || r.off >= r.size {
		return n, nil
	}
	outOff := r.off - wavHeaderLen
	first := outOff / 2
	skip := outOff % 2
	count := min(int64(len(p))/2+1, (r.size-r.off+1)/2+1, 1<<16)
	in := make([]byte, count*r.inWidth)
	got, err := r.src.ReadAt(in, first*r.inWidth)
	if got == 0 && err != nil {
		if errors.Is(err, io.EOF) {
			return n, io.EOF
		}
		return n, err
	}
	samples := int64(got) / r.inWidth
	out := make([]byte, samples*2)
	for i := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(r.sample(in[i*r.inWidth:])))
	}
	out = out[skip:]
	if rem := r.size - r.off; int64(len(out)) > rem {
		out = out[:rem] // ignore a trailing partial frame in the source
	}
	m := copy(p, out)
	r.off += int64(m)
	return n + m, nil
}

func (r *wavReader) sample(b []byte) int16 {
	switch r.format.Encoding {
	case "pcm16":
		return int16(binary.LittleEndian.Uint16(b))
	case "mulaw":
		return mulawTable[b[0]]
	case "alaw":
		return alawTable[b[0]]
	case "u8":
		return int16(int32(b[0])-128) << 8
	default: // f32le
		v := math.Float32frombits(binary.LittleEndian.Uint32(b))
		return int16(max(-32768, min(32767, math.Round(float64(v)*32767))))
	}
}
