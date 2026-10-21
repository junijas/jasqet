package main

// Automatic subtitles: Jasqet listens to a video or a recording and writes
// what's said as an .srt next to it, with Whisper (whisper.cpp) running on this
// computer. Nothing is uploaded. The speech engine (about 9 MB) and its
// language model (78 MB) download once, the first time you ask, and are
// checked against the fixed checksums below before they're used.

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Whisper build and model Jasqet uses, with their SHA-256 (as GitHub
// and Hugging Face publish them). Tests use their own server.
var whisperBuild = "https://github.com/ggml-org/whisper.cpp/releases/download/b5454/"

var whisperZips = map[string]struct{ name, sum string }{
	"windows/amd64": {"whisper-bin-x64.zip", "6ba69e3482d7826214f90a6a9c84ca07782aec1e1d0c6a7c30c994fd5d816ccb"},
	"windows/arm64": {"whisper-bin-win-cpu-arm64.zip", "28c37e7b598c3d9bbfef94f3bd67f2ee6f12b7c86f3da5edcf5e4308d615ff6d"},
}

// the "base" model, 8-bit: almost as good as the full one at half its size
var whisperModel = struct{ url, name, sum string }{
	"https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base-q8_0.bin", "ggml-base-q8_0.bin",
	"c577b9a86e7e048a0b7eada054f4dd79a56bbfa911fbdacf900ac5b567cbb7d9",
}

const whisperSetupMB = 87

var errWhisperMissing = errors.New("Automatic subtitles need whisper.cpp on this computer (on a Mac: brew install whisper-cpp)")

func whisperDir() string       { return filepath.Join(toolsDir(), "whisper") }
func whisperModelPath() string { return filepath.Join(whisperDir(), whisperModel.name) }

// whisperCLI finds the speech engine: Jasqet's own copy on Windows, the one
// installed on the computer elsewhere.
func whisperCLI() string {
	if p := filepath.Join(whisperDir(), exeName("whisper-cli")); fileExists(p) {
		return p
	}
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath("whisper-cli"); err == nil {
			return p
		}
	}
	return ""
}

func whisperReady() bool { return whisperCLI() != "" && fileExists(whisperModelPath()) }

// SubsJob: making subtitles for one download, as the window shows it.
type SubsJob struct {
	State     string  `json:"state"` // "waiting", "setup", "audio", "listening", "done", "error"
	Progress  float64 `json:"progress"`
	Message   string  `json:"message,omitempty"`
	File      string  `json:"file,omitempty"` // the .srt it made
	Translate bool    `json:"translate,omitempty"`
	at        time.Time
}

type aiState struct {
	mu    sync.Mutex
	jobs  map[string]*SubsJob // by download
	setup sync.Mutex          // one setup at a time
	turn  chan struct{}       // one file at a time: listening takes the whole processor
}

func newAIState() *aiState {
	return &aiState{jobs: map[string]*SubsJob{}, turn: make(chan struct{}, 1)}
}

// jobsView: the jobs to show (finished ones for ten minutes).
func (a *aiState) jobsView() map[string]SubsJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]SubsJob{}
	for id, j := range a.jobs {
		if (j.State == "done" || j.State == "error") && time.Since(j.at) > 10*time.Minute {
			delete(a.jobs, id)
			continue
		}
		out[id] = *j
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// speechExts: what can be listened to (the sound is taken out first).
var speechExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true, ".wmv": true, ".3gp": true,
	".flv": true, ".ts": true, ".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".flac": true, ".wma": true,
	".ogg": true, ".oga": true, ".opus": true, ".mka": true,
}

// MakeSubtitles starts writing down what's said in a finished download
// (translated into English if asked).
func (m *Manager) MakeSubtitles(d *Download, translate bool) error {
	d.mu.Lock()
	done, path, id := d.Status == StDone, filepath.Join(d.Dir, d.FileName), d.ID
	d.mu.Unlock()
	switch {
	case !done:
		return errors.New("subtitles can be made once it has finished downloading")
	case !speechExts[strings.ToLower(filepath.Ext(path))]:
		return errors.New("only videos and recordings have speech to write down")
	case !fileExists(path):
		return errors.New("the file isn't there any more")
	case runtime.GOOS != "windows" && whisperCLI() == "":
		return errWhisperMissing
	}
	a := m.ai
	a.mu.Lock()
	if j := a.jobs[id]; j != nil && j.State != "done" && j.State != "error" {
		a.mu.Unlock()
		return nil // already on it
	}
	j := &SubsJob{State: "waiting", Message: "Waiting for the other subtitles to finish", Translate: translate, at: time.Now()}
	a.jobs[id] = j
	a.mu.Unlock()
	go m.subsJob(id, path, j)
	return nil
}

func (m *Manager) subsJob(id, path string, j *SubsJob) {
	a := m.ai
	set := func(state string, p float64, msg string) {
		a.mu.Lock()
		j.State, j.Progress, j.Message, j.at = state, p, msg, time.Now()
		a.mu.Unlock()
	}
	a.turn <- struct{}{}
	defer func() { <-a.turn }()
	ctx := context.Background()
	err := func() error {
		if !whisperReady() {
			set("setup", 0, fmt.Sprintf("Setting up automatic subtitles, one time only (about %d MB)", whisperSetupMB))
			if err := m.installWhisper(ctx, func(p float64, msg string) { set("setup", p, msg) }); err != nil {
				return fmt.Errorf("couldn't set up automatic subtitles: %w", err)
			}
		}
		work := filepath.Join(os.TempDir(), "Jasqet-subs")
		if err := os.MkdirAll(work, 0o755); err != nil {
			return err
		}
		base := filepath.Join(work, id)
		audio := base + ".wav"
		defer os.Remove(audio)
		const msg = "Taking the sound out…"
		set("audio", 0, msg)
		if err := decodeSpeech(ctx, path, audio, func(p float64) { set("audio", p, msg) }); err != nil {
			return err
		}
		set("listening", 0, "Finding the language…")
		lang := detectLanguage(ctx, audio, base)
		set("listening", 0, "Listening…")
		defer os.Remove(base + ".srt")
		if err := runWhisper(ctx, audio, base, lang, j.Translate, func(p float64) { set("listening", p, "Listening…") }); err != nil {
			return err
		}
		out := subsName(path, lang, j.Translate)
		if err := moveFile(base+".srt", out); err != nil {
			return err
		}
		a.mu.Lock()
		j.State, j.Progress, j.Message, j.File, j.at = "done", 1, "", filepath.Base(out), time.Now()
		a.mu.Unlock()
		return nil
	}()
	if err != nil {
		set("error", 0, friendlyError(err))
		return
	}
	m.notifyDesktop("📝 Subtitles ready: "+filepath.Base(path), id)
}

// installWhisper downloads the speech engine and its model, checks them and
// puts them in Jasqet's tools folder.
func (m *Manager) installWhisper(ctx context.Context, progress func(float64, string)) error {
	m.ai.setup.Lock()
	defer m.ai.setup.Unlock()
	dir := whisperDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	type part struct {
		label, url, dest, sum string
		zip                   bool
		share                 float64 // of the whole setup, by size
	}
	var parts []part
	if whisperCLI() == "" {
		z, ok := whisperZips[runtime.GOOS+"/"+runtime.GOARCH]
		if !ok {
			return errWhisperMissing
		}
		parts = append(parts, part{"the speech engine", whisperBuild + z.name, filepath.Join(dir, "engine.zip"), z.sum, true, 0.1})
	}
	if !fileExists(whisperModelPath()) {
		parts = append(parts, part{"the language model", whisperModel.url, whisperModelPath(), whisperModel.sum, false, 0.9})
	}
	var base, total float64
	for _, p := range parts {
		total += p.share
	}
	for _, p := range parts {
		share := p.share / total
		tmp := p.dest + ".part"
		err := m.fetchToFile(ctx, p.url, tmp, func(done, size int64) {
			if size > 0 {
				progress(base+share*float64(done)/float64(size), fmt.Sprintf("Setting up automatic subtitles, one time only: %s, %s of %s", p.label, humanSize(done), humanSize(size)))
			}
		})
		if err != nil {
			return err
		}
		if got, err := hashFile(ctx, tmp, "sha256"); err != nil {
			return err
		} else if got != p.sum {
			os.Remove(tmp) // damaged or changed: never run it
			return fmt.Errorf("%s doesn't match its published checksum. Try again", p.label)
		}
		if p.zip {
			err = unzipWhisper(tmp, dir)
			os.Remove(tmp)
		} else {
			err = os.Rename(tmp, p.dest)
		}
		if err != nil {
			return err
		}
		base += share
	}
	return nil
}

// whisperFiles: what Jasqet takes from the engine's zip (the program and
// its libraries, not the demos).
var whisperFiles = regexp.MustCompile(`(?i)^(whisper-cli(\.exe)?|whisper\.dll|ggml[a-z0-9-]*\.dll)$`)

func unzipWhisper(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		name := filepath.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if f.FileInfo().IsDir() || !whisperFiles.MatchString(name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, name)
		out, err := os.OpenFile(dest+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err == nil {
			_, err = io.Copy(out, io.LimitReader(rc, 64<<20))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		rc.Close()
		if err == nil {
			os.Remove(dest)
			err = os.Rename(dest+".tmp", dest)
		}
		if err != nil {
			return err
		}
		found = found || strings.HasPrefix(strings.ToLower(name), "whisper-cli")
	}
	if !found {
		return errors.New("the speech engine wasn't in its download")
	}
	return nil
}

// decodeSpeech takes the sound out of a file as 16 kHz mono WAV, what
// Whisper listens to: with Windows' own decoders, else ffmpeg if it's there.
func decodeSpeech(ctx context.Context, in, out string, progress func(float64)) error {
	err := decodeAudioOS(ctx, in, out, progress)
	if err == nil {
		return nil
	}
	if ff := ffmpegPath(); ff != "" {
		cmd := exec.CommandContext(ctx, ff, "-nostdin", "-y", "-i", in, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out)
		hideWindow(cmd)
		if cmd.Run() == nil {
			return nil
		}
	}
	return fmt.Errorf("couldn't take the sound out of this file (%v)", err)
}

var (
	whisperProgressRe = regexp.MustCompile(`progress =\s*(\d+)%`)
	whisperLangRe     = regexp.MustCompile(`auto-detected language: ([a-z]{2,3}) \(p = ([0-9.]+)\)`)
)

// whisperThreads leaves two of the processor's threads for everything else.
func whisperThreads() int { return max(1, min(8, runtime.NumCPU()-2)) }

func whisperCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, whisperCLI(), append([]string{"-m", whisperModelPath(), "-t", strconv.Itoa(whisperThreads())}, args...)...)
	hideWindow(cmd)
	lowPriority(cmd) // the computer stays usable meanwhile
	return cmd
}

// detectLanguage asks Whisper which language is spoken in up to five
// 30-second samples spread over the recording, and takes the one it's most
// sure of overall. Its guess from the first 30 seconds alone can be wrong (an
// accent, a jingle), and a wrong guess spoils the whole transcript. "" = let
// Whisper decide as it goes.
func detectLanguage(ctx context.Context, wav, tmpBase string) string {
	st, err := os.Stat(wav)
	if err != nil || whisperCLI() == "" {
		return ""
	}
	secs := (st.Size() - 44) / (2 * speechRate)
	n := min(5, max(1, secs/30))
	var files []string
	for i := int64(0); i < n; i++ {
		f := fmt.Sprintf("%s-lang%d.wav", tmpBase, i)
		if cutSpeechWAV(wav, f, max(0, secs*(2*i+1)/(2*n)-15), 30) == nil {
			files = append(files, f)
			defer os.Remove(f)
		}
	}
	out, _ := whisperCmd(ctx, append([]string{"-l", "auto", "-dl"}, files...)...).CombinedOutput()
	return pickLanguage(string(out))
}

// pickLanguage adds up how sure Whisper was of each language it heard.
func pickLanguage(out string) string {
	score := map[string]float64{}
	best := ""
	for _, m := range whisperLangRe.FindAllStringSubmatch(out, -1) {
		p, _ := strconv.ParseFloat(m[2], 64)
		score[m[1]] += p
		if best == "" || score[m[1]] > score[best] {
			best = m[1]
		}
	}
	return best
}

// runWhisper listens to a 16 kHz WAV and writes outBase.srt, in lang ("" = auto).
func runWhisper(ctx context.Context, audio, outBase, lang string, translate bool, progress func(float64)) error {
	if whisperCLI() == "" {
		return errWhisperMissing
	}
	if lang == "" {
		lang = "auto"
	}
	args := []string{"-f", audio, "-l", lang, "-osrt", "-of", outBase, "-pp"}
	if translate {
		args = append(args, "-tr")
	}
	cmd := whisperCmd(ctx, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var last string
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		line := sc.Text()
		if m := whisperProgressRe.FindStringSubmatch(line); m != nil {
			p, _ := strconv.Atoi(m[1])
			progress(float64(p) / 100)
		} else if strings.TrimSpace(line) != "" {
			last = strings.TrimSpace(line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("the speech engine stopped: %s", last)
	}
	if !fileExists(outBase + ".srt") {
		return errors.New("the speech engine wrote no subtitles")
	}
	return nil
}

// subsName: "Lecture.English (auto-generated).srt" next to "Lecture.mp4"
// (the viewer shows it as "English (auto-generated)"). A translation is
// "English (auto-translated)"; an unknown language is just "Auto-generated".
func subsName(media, lang string, translate bool) string {
	label := langName(lang)
	switch {
	case translate:
		label = "English (auto-translated)"
	case label == "":
		label = "Auto-generated"
	default:
		label += " (auto-generated)"
	}
	return strings.TrimSuffix(media, filepath.Ext(media)) + "." + label + ".srt"
}
