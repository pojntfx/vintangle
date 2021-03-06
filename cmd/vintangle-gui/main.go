package main

//go:generate glib-compile-schemas .

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	jsoniter "github.com/json-iterator/go"
	"github.com/phayes/freeport"
	"github.com/pojntfx/htorrent/pkg/client"
	"github.com/pojntfx/htorrent/pkg/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	_ "embed"
)

type media struct {
	name string
	size int
}

type mediaWithPriority struct {
	media
	priority int
}

type mpvCommand struct {
	Command []interface{} `json:"command"`
}

type mpvFloat64Response struct {
	Data float64 `json:"data"`
}

var (
	//go:embed assistant.ui
	assistantUI string

	//go:embed controls.ui
	controlsUI string

	//go:embed description.ui
	descriptionUI string

	//go:embed warning.ui
	warningUI string

	//go:embed error.ui
	errorUI string

	//go:embed menu.ui
	menuUI string

	//go:embed about.ui
	aboutUI string

	//go:embed preferences.ui
	preferencesUI string

	//go:embed subtitles.ui
	subtitlesUI string

	//go:embed preparing.ui
	preparingUI string

	//go:embed style.css
	styleCSS string

	//go:embed gschemas.compiled
	geschemas []byte

	letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

	json = jsoniter.ConfigCompatibleWithStandardLibrary

	errKilled            = errors.New("signal: killed")
	errNoWorkingMPVFound = errors.New("could not find working a working mpv")
)

const (
	appID   = "com.pojtinger.felicitas.vintangle"
	stateID = appID + ".state"

	welcomePageName = "welcome-page"
	mediaPageName   = "media-page"
	readyPageName   = "ready-page"

	playIcon  = "media-playback-start-symbolic"
	pauseIcon = "media-playback-pause-symbolic"

	readmePlaceholder = "No README found."

	verboseFlag = "verbose"
	storageFlag = "storage"
	mpvFlag     = "mpv"

	gatewayRemoteFlag   = "gatewayremote"
	gatewayURLFlag      = "gatewayurl"
	gatewayUsernameFlag = "gatewayusername"
	gatewayPasswordFlag = "gatewaypassword"

	keycodeEscape = 66

	schemaDirEnvVar = "GSETTINGS_SCHEMA_DIR"

	preferencesActionName      = "preferences"
	applyPreferencesActionName = "applypreferences"

	mpvFlathubURL = "https://flathub.org/apps/details/io.mpv.Mpv"
	mpvWebsiteURL = "https://mpv.io/installation/"

	issuesURL = "https://github.com/pojntfx/vintangle/issues"
)

// See https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-go/22892986#22892986
func randSeq(n int) string {
	b := make([]rune, n)

	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}

	return string(b)
}

func getStreamURL(base string, magnet, path string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	streamSuffix, err := url.Parse("/stream")
	if err != nil {
		return "", err
	}

	stream := baseURL.ResolveReference(streamSuffix)

	q := stream.Query()
	q.Set("magnet", magnet)
	q.Set("path", path)
	stream.RawQuery = q.Encode()

	return stream.String(), nil
}

func formatDuration(duration time.Duration) string {
	hours := math.Floor(duration.Hours())
	minutes := math.Floor(duration.Minutes()) - (hours * 60)
	seconds := math.Floor(duration.Seconds()) - (minutes * 60) - (hours * 3600)

	return fmt.Sprintf("%02d:%02d:%02d", int(hours), int(minutes), int(seconds))
}

func getDisplayPathWithoutRoot(p string) string {
	parts := strings.Split(p, "/") // Incoming paths are always UNIX

	if len(parts) < 2 {
		return p
	}

	return filepath.Join(parts[1:]...) // Outgoing paths are OS-specific (display only)
}

func findWorkingMPV() (string, error) {
	if _, err := os.Stat("/.flatpak-info"); err == nil {
		if err := exec.Command("flatpak-spawn", "--host", "mpv", "--version").Run(); err == nil {
			return "flatpak-spawn --host mpv", nil
		}

		if err := exec.Command("flatpak-spawn", "--host", "flatpak", "run", "io.mpv.Mpv", "--version").Run(); err == nil {
			return "flatpak-spawn --host flatpak run io.mpv.Mpv", nil
		}

		return "", errNoWorkingMPVFound
	}

	if err := exec.Command("mpv", "--version").Run(); err == nil {
		return "mpv", nil
	}

	if err := exec.Command("flatpak", "run", "io.mpv.Mpv", "--version").Run(); err == nil {
		return "flatpak run io.mpv.Mpv", nil
	}

	return "", errNoWorkingMPVFound
}

func openAssistantWindow(ctx context.Context, app *adw.Application, manager *client.Manager, apiAddr, apiUsername, apiPassword string, settings *gio.Settings, gateway *server.Gateway, cancel func(), tmpDir string) error {
	app.StyleManager().SetColorScheme(adw.ColorSchemeDefault)

	builder := gtk.NewBuilderFromString(assistantUI, len(assistantUI))

	window := builder.GetObject("main-window").Cast().(*adw.ApplicationWindow)
	overlay := builder.GetObject("toast-overlay").Cast().(*adw.ToastOverlay)
	buttonHeaderbarTitle := builder.GetObject("button-headerbar-title").Cast().(*gtk.Label)
	buttonHeaderbarSubtitle := builder.GetObject("button-headerbar-subtitle").Cast().(*gtk.Label)
	previousButton := builder.GetObject("previous-button").Cast().(*gtk.Button)
	nextButton := builder.GetObject("next-button").Cast().(*gtk.Button)
	menuButton := builder.GetObject("menu-button").Cast().(*gtk.MenuButton)
	headerbarSpinner := builder.GetObject("headerbar-spinner").Cast().(*gtk.Spinner)
	stack := builder.GetObject("stack").Cast().(*gtk.Stack)
	magnetLinkEntry := builder.GetObject("magnet-link-entry").Cast().(*gtk.Entry)
	mediaSelectionGroup := builder.GetObject("media-selection-group").Cast().(*adw.PreferencesGroup)
	rightsConfirmationButton := builder.GetObject("rights-confirmation-button").Cast().(*gtk.CheckButton)
	playButton := builder.GetObject("play-button").Cast().(*gtk.Button)
	mediaInfoDisplay := builder.GetObject("media-info-display").Cast().(*gtk.Box)
	mediaInfoButton := builder.GetObject("media-info-button").Cast().(*gtk.Button)

	descriptionBuilder := gtk.NewBuilderFromString(descriptionUI, len(descriptionUI))
	descriptionWindow := descriptionBuilder.GetObject("description-window").Cast().(*adw.Window)
	descriptionText := descriptionBuilder.GetObject("description-text").Cast().(*gtk.TextView)

	warningBuilder := gtk.NewBuilderFromString(warningUI, len(warningUI))
	warningDialog := warningBuilder.GetObject("warning-dialog").Cast().(*gtk.MessageDialog)
	mpvFlathubDownloadButton := warningBuilder.GetObject("mpv-download-flathub-button").Cast().(*gtk.Button)
	mpvWebsiteDownloadButton := warningBuilder.GetObject("mpv-download-website-button").Cast().(*gtk.Button)
	mpvManualConfigurationButton := warningBuilder.GetObject("mpv-manual-configuration-button").Cast().(*gtk.Button)

	torrentTitle := ""
	torrentMedia := []media{}
	torrentReadme := ""

	selectedTorrentMedia := ""
	activators := []*gtk.CheckButton{}
	mediaRows := []*adw.ActionRow{}

	subtitles := []mediaWithPriority{}

	stack.SetVisibleChildName(welcomePageName)

	magnetLinkEntry.ConnectChanged(func() {
		selectedTorrentMedia = ""
		for _, activator := range activators {
			activator.SetActive(false)
		}

		if magnetLinkEntry.Text() == "" {
			nextButton.SetSensitive(false)

			return
		}

		nextButton.SetSensitive(true)
	})

	onNext := func() {
		switch stack.VisibleChildName() {
		case welcomePageName:
			if selectedTorrentMedia == "" {
				nextButton.SetSensitive(false)
			}

			headerbarSpinner.SetSpinning(true)
			magnetLinkEntry.SetSensitive(false)

			go func() {
				magnetLink := magnetLinkEntry.Text()

				log.Info().
					Str("magnetLink", magnetLink).
					Msg("Getting info for magnet link")

				info, err := manager.GetInfo(magnetLink)
				if err != nil {
					log.Warn().
						Str("magnetLink", magnetLink).
						Err(err).
						Msg("Could not get info for magnet link")

					toast := adw.NewToast("Could not get info for this magnet link.")

					overlay.AddToast(toast)

					headerbarSpinner.SetSpinning(false)
					magnetLinkEntry.SetSensitive(true)

					magnetLinkEntry.GrabFocus()

					return
				}

				torrentTitle = info.Name
				torrentReadme = info.Description
				torrentMedia = []media{}
				for _, file := range info.Files {
					torrentMedia = append(torrentMedia, media{
						name: file.Path,
						size: int(file.Length),
					})
				}

				for _, row := range mediaRows {
					mediaSelectionGroup.Remove(row)
				}
				mediaRows = []*adw.ActionRow{}

				activators = []*gtk.CheckButton{}
				for i, file := range torrentMedia {
					row := adw.NewActionRow()

					activator := gtk.NewCheckButton()

					if len(activators) > 0 {
						activator.SetGroup(activators[i-1])
					}
					activators = append(activators, activator)

					m := file.name
					activator.SetActive(false)
					activator.ConnectActivate(func() {
						if m != selectedTorrentMedia {
							selectedTorrentMedia = m

							rightsConfirmationButton.SetActive(false)
						}

						nextButton.SetSensitive(true)
					})

					row.SetTitle(getDisplayPathWithoutRoot(file.name))
					row.SetSubtitle(fmt.Sprintf("%v MB", file.size/1000/1000))
					row.SetActivatable(true)

					row.AddPrefix(activator)
					row.SetActivatableWidget(activator)

					mediaRows = append(mediaRows, row)
					mediaSelectionGroup.Add(row)
				}

				headerbarSpinner.SetSpinning(false)
				magnetLinkEntry.SetSensitive(true)
				previousButton.SetVisible(true)

				buttonHeaderbarTitle.SetLabel(torrentTitle)

				mediaInfoDisplay.SetVisible(false)
				mediaInfoButton.SetVisible(true)

				descriptionText.SetWrapMode(gtk.WrapWord)
				if !utf8.Valid([]byte(torrentReadme)) || strings.TrimSpace(torrentReadme) == "" {
					descriptionText.Buffer().SetText(readmePlaceholder)
				} else {
					descriptionText.Buffer().SetText(torrentReadme)
				}

				stack.SetVisibleChildName(mediaPageName)
			}()
		case mediaPageName:
			nextButton.SetVisible(false)

			buttonHeaderbarSubtitle.SetVisible(true)
			buttonHeaderbarSubtitle.SetLabel(getDisplayPathWithoutRoot(selectedTorrentMedia))

			stack.SetVisibleChildName(readyPageName)
		}
	}

	onPrevious := func() {
		switch stack.VisibleChildName() {
		case mediaPageName:
			previousButton.SetVisible(false)
			nextButton.SetSensitive(true)

			mediaInfoDisplay.SetVisible(true)
			mediaInfoButton.SetVisible(false)

			stack.SetVisibleChildName(welcomePageName)
		case readyPageName:
			nextButton.SetVisible(true)

			buttonHeaderbarSubtitle.SetVisible(false)

			stack.SetVisibleChildName(mediaPageName)
		}
	}

	magnetLinkEntry.ConnectActivate(onNext)
	nextButton.ConnectClicked(onNext)
	previousButton.ConnectClicked(onPrevious)

	preferencesWindow, mpvCommandInput := addMainMenu(ctx, app, window, settings, menuButton, overlay, gateway, cancel)

	mediaInfoButton.ConnectClicked(func() {
		descriptionWindow.Show()
	})

	ctrl := gtk.NewEventControllerKey()
	descriptionWindow.AddController(ctrl)
	descriptionWindow.SetTransientFor(&window.Window)

	descriptionWindow.ConnectCloseRequest(func() (ok bool) {
		descriptionWindow.Close()
		descriptionWindow.SetVisible(false)

		return ok
	})

	ctrl.ConnectKeyReleased(func(keyval, keycode uint, state gdk.ModifierType) {
		if keycode == keycodeEscape {
			descriptionWindow.Close()
			descriptionWindow.SetVisible(false)
		}
	})

	rightsConfirmationButton.ConnectToggled(func() {
		if rightsConfirmationButton.Active() {
			playButton.AddCSSClass("suggested-action")
			playButton.SetSensitive(true)

			return
		}

		playButton.RemoveCSSClass("suggested-action")
		playButton.SetSensitive(false)
	})

	playButton.ConnectClicked(func() {
		window.Close()

		subtitles = []mediaWithPriority{}
		for _, media := range torrentMedia {
			if media.name != selectedTorrentMedia {
				if strings.HasSuffix(media.name, ".srt") || strings.HasSuffix(media.name, ".vtt") || strings.HasSuffix(media.name, ".ass") {
					subtitles = append(subtitles, mediaWithPriority{
						media:    media,
						priority: 0,
					})
				} else {
					subtitles = append(subtitles, mediaWithPriority{
						media:    media,
						priority: 1,
					})
				}
			}
		}

		if err := openControlsWindow(ctx, app, torrentTitle, subtitles, selectedTorrentMedia, torrentReadme, manager, apiAddr, apiUsername, apiPassword, magnetLinkEntry.Text(), settings, gateway, cancel, tmpDir); err != nil {
			panic(err)
		}
	})

	if runtime.GOOS == "linux" {
		mpvFlathubDownloadButton.SetVisible(true)
		warningDialog.SetDefaultWidget(mpvFlathubDownloadButton)
	} else {
		warningDialog.SetDefaultWidget(mpvWebsiteDownloadButton)
	}

	mpvFlathubDownloadButton.ConnectClicked(func() {
		gtk.ShowURIFull(ctx, &window.Window, mpvFlathubURL, gdk.CURRENT_TIME, func(res gio.AsyncResulter) {
			warningDialog.Close()

			os.Exit(0)
		})
	})

	mpvWebsiteDownloadButton.ConnectClicked(func() {
		gtk.ShowURIFull(ctx, &window.Window, mpvWebsiteURL, gdk.CURRENT_TIME, func(res gio.AsyncResulter) {
			warningDialog.Close()

			os.Exit(0)
		})
	})

	mpvManualConfigurationButton.ConnectClicked(func() {
		warningDialog.Close()

		preferencesWindow.Show()
		mpvCommandInput.GrabFocus()
	})

	warningDialog.SetTransientFor(&window.Window)
	warningDialog.ConnectCloseRequest(func() (ok bool) {
		warningDialog.Close()
		warningDialog.SetVisible(false)

		return ok
	})

	app.AddWindow(&window.Window)

	window.ConnectShow(func() {
		if oldMPVCommand := settings.String(mpvFlag); strings.TrimSpace(oldMPVCommand) == "" {
			newMPVCommand, err := findWorkingMPV()
			if err != nil {
				warningDialog.Show()

				return
			}

			settings.SetString(mpvFlag, newMPVCommand)
			settings.Apply()
		}

		magnetLinkEntry.GrabFocus()
	})

	window.Show()

	return nil
}

func openControlsWindow(ctx context.Context, app *adw.Application, torrentTitle string, subtitles []mediaWithPriority, selectedTorrentMedia, torrentReadme string, manager *client.Manager, apiAddr, apiUsername, apiPassword, magnetLink string, settings *gio.Settings, gateway *server.Gateway, cancel func(), tmpDir string) error {
	app.StyleManager().SetColorScheme(adw.ColorSchemePreferDark)

	builder := gtk.NewBuilderFromString(controlsUI, len(controlsUI))

	window := builder.GetObject("main-window").Cast().(*adw.ApplicationWindow)
	overlay := builder.GetObject("toast-overlay").Cast().(*adw.ToastOverlay)
	buttonHeaderbarTitle := builder.GetObject("button-headerbar-title").Cast().(*gtk.Label)
	buttonHeaderbarSubtitle := builder.GetObject("button-headerbar-subtitle").Cast().(*gtk.Label)
	playButton := builder.GetObject("play-button").Cast().(*gtk.Button)
	stopButton := builder.GetObject("stop-button").Cast().(*gtk.Button)
	volumeButton := builder.GetObject("volume-button").Cast().(*gtk.VolumeButton)
	subtitleButton := builder.GetObject("subtitle-button").Cast().(*gtk.Button)
	fullscreenButton := builder.GetObject("fullscreen-button").Cast().(*gtk.ToggleButton)
	mediaInfoButton := builder.GetObject("media-info-button").Cast().(*gtk.Button)
	menuButton := builder.GetObject("menu-button").Cast().(*gtk.MenuButton)
	copyButton := builder.GetObject("copy-button").Cast().(*gtk.Button)
	elapsedTrackLabel := builder.GetObject("elapsed-track-label").Cast().(*gtk.Label)
	remainingTrackLabel := builder.GetObject("remaining-track-label").Cast().(*gtk.Label)
	seeker := builder.GetObject("seeker").Cast().(*gtk.Scale)

	descriptionBuilder := gtk.NewBuilderFromString(descriptionUI, len(descriptionUI))
	descriptionWindow := descriptionBuilder.GetObject("description-window").Cast().(*adw.Window)
	descriptionText := descriptionBuilder.GetObject("description-text").Cast().(*gtk.TextView)

	subtitlesBuilder := gtk.NewBuilderFromString(subtitlesUI, len(subtitlesUI))
	subtitlesDialog := subtitlesBuilder.GetObject("subtitles-dialog").Cast().(*gtk.Dialog)
	subtitlesCancelButton := subtitlesBuilder.GetObject("button-cancel").Cast().(*gtk.Button)
	subtitlesOKButton := subtitlesBuilder.GetObject("button-ok").Cast().(*gtk.Button)
	subtitlesSelectionGroup := subtitlesBuilder.GetObject("subtitle-tracks").Cast().(*adw.PreferencesGroup)
	addSubtitlesFromFileButton := subtitlesBuilder.GetObject("add-from-file-button").Cast().(*gtk.Button)

	preparingBuilder := gtk.NewBuilderFromString(preparingUI, len(preparingUI))
	preparingWindow := preparingBuilder.GetObject("preparing-window").Cast().(*adw.Window)
	preparingCancelButton := preparingBuilder.GetObject("cancel-preparing-button").Cast().(*gtk.Button)

	buttonHeaderbarTitle.SetLabel(torrentTitle)
	buttonHeaderbarSubtitle.SetLabel(getDisplayPathWithoutRoot(selectedTorrentMedia))

	copyButton.ConnectClicked(func() {
		window.Clipboard().SetText(magnetLink)
	})

	stopButton.ConnectClicked(func() {
		window.Close()

		if err := openAssistantWindow(ctx, app, manager, apiAddr, apiUsername, apiPassword, settings, gateway, cancel, tmpDir); err != nil {
			openErrorDialog(ctx, window, err)

			return
		}
	})

	mediaInfoButton.ConnectClicked(func() {
		descriptionWindow.Show()
	})

	ctrl := gtk.NewEventControllerKey()
	descriptionWindow.AddController(ctrl)
	descriptionWindow.SetTransientFor(&window.Window)

	descriptionWindow.ConnectCloseRequest(func() (ok bool) {
		descriptionWindow.Close()
		descriptionWindow.SetVisible(false)

		return ok
	})

	ctrl.ConnectKeyReleased(func(keyval, keycode uint, state gdk.ModifierType) {
		if keycode == keycodeEscape {
			descriptionWindow.Close()
			descriptionWindow.SetVisible(false)
		}
	})

	descriptionText.SetWrapMode(gtk.WrapWord)
	if !utf8.Valid([]byte(torrentReadme)) || strings.TrimSpace(torrentReadme) == "" {
		descriptionText.Buffer().SetText(readmePlaceholder)
	} else {
		descriptionText.Buffer().SetText(torrentReadme)
	}

	preparingWindow.SetTransientFor(&window.Window)

	preparingWindow.ConnectCloseRequest(func() (ok bool) {
		preparingWindow.Close()
		preparingWindow.SetVisible(false)

		return ok
	})

	preparingCancelButton.ConnectClicked(func() {
		preparingWindow.Close()

		window.Close()

		if err := openAssistantWindow(ctx, app, manager, apiAddr, apiUsername, apiPassword, settings, gateway, cancel, tmpDir); err != nil {
			openErrorDialog(ctx, window, err)

			return
		}
	})

	usernameAndPassword := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%v:%v", apiUsername, apiPassword)))

	streamURL, err := getStreamURL(apiAddr, magnetLink, selectedTorrentMedia)
	if err != nil {
		return err
	}

	ipcDir, err := os.MkdirTemp(os.TempDir(), "mpv-ipc")
	if err != nil {
		return err
	}

	ipcFile := filepath.Join(ipcDir, "mpv.sock")

	shell := []string{"sh", "-c"}
	if runtime.GOOS == "windows" {
		shell = []string{"cmd", "/c"}
	}
	commandLine := append(shell, fmt.Sprintf("%v '--keep-open=always' '--no-osc' '--no-input-default-bindings' '--pause' '--input-ipc-server=%v' '--http-header-fields=Authorization: Basic %v' '%v'", settings.String(mpvFlag), ipcFile, usernameAndPassword, streamURL))

	command := exec.Command(
		commandLine[0],
		commandLine[1:]...,
	)
	if runtime.GOOS != "windows" {
		command.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true,
		}
	}

	addMainMenu(ctx, app, window, settings, menuButton, overlay, gateway, func() {
		cancel()

		if command.Process != nil {
			if err := command.Process.Kill(); err != nil {
				openErrorDialog(ctx, window, err)

				return
			}
		}
	})

	app.AddWindow(&window.Window)

	window.ConnectShow(func() {
		preparingWindow.Show()

		if err := command.Start(); err != nil {
			openErrorDialog(ctx, window, err)

			return
		}

		window.ConnectCloseRequest(func() (ok bool) {
			if command.Process != nil {
				if runtime.GOOS == "windows" {
					if err := command.Process.Kill(); err != nil {
						openErrorDialog(ctx, window, err)

						return false
					}
				} else {
					if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
						openErrorDialog(ctx, window, err)

						return false
					}
				}
			}

			if err := os.RemoveAll(ipcDir); err != nil {
				openErrorDialog(ctx, window, err)

				return false
			}

			return true
		})

		var sock net.Conn
		for {
			sock, err = net.Dial("unix", ipcFile)
			if err == nil {
				break
			}

			time.Sleep(time.Millisecond * 100)

			log.Error().
				Str("path", ipcFile).
				Err(err).
				Msg("Could not dial IPC socket, retrying in 100ms")
		}

		encoder := json.NewEncoder(sock)
		if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "volume", 100}}); err != nil {
			openErrorDialog(ctx, window, err)

			return
		}

		activators := []*gtk.CheckButton{}

		for i, file := range append(
			[]mediaWithPriority{
				{media: media{
					name: "None",
					size: 0,
				},
					priority: -1,
				},
			},
			subtitles...) {
			row := adw.NewActionRow()

			activator := gtk.NewCheckButton()

			if len(activators) > 0 {
				activator.SetGroup(activators[i-1])
			}
			activators = append(activators, activator)

			m := file.name
			j := i
			activator.SetActive(false)
			activator.ConnectActivate(func() {
				if j == 0 {
					log.Info().
						Msg("Disabling subtitles")

					if err := encoder.Encode(mpvCommand{[]interface{}{"change-list", "sub-files", "clr"}}); err != nil {
						openErrorDialog(ctx, window, err)

						return
					}

					return
				}

				streamURL, err := getStreamURL(apiAddr, magnetLink, m)
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				log.Info().
					Str("streamURL", streamURL).
					Msg("Downloading subtitles")

				hc := &http.Client{}

				req, err := http.NewRequest(http.MethodGet, streamURL, http.NoBody)
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}
				req.SetBasicAuth(apiUsername, apiPassword)

				res, err := hc.Do(req)
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}
				if res.Body != nil {
					defer res.Body.Close()
				}
				if res.StatusCode != http.StatusOK {
					openErrorDialog(ctx, window, errors.New(res.Status))

					return
				}

				subtitlesFile := filepath.Join(tmpDir, path.Base(m))
				f, err := os.Create(subtitlesFile)
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				if _, err := io.Copy(f, res.Body); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				log.Info().
					Str("path", subtitlesFile).
					Msg("Setting subtitles")

				if err := encoder.Encode(mpvCommand{[]interface{}{"change-list", "sub-files", "set", subtitlesFile}}); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}
			})

			if i == 0 {
				row.SetTitle(file.name)
				row.SetSubtitle("Disable subtitles")
			} else if file.priority == 0 {
				row.SetTitle(getDisplayPathWithoutRoot(file.name))
				row.SetSubtitle("Integrated subtitle")
			} else {
				row.SetTitle(getDisplayPathWithoutRoot(file.name))
				row.SetSubtitle("Extra file from media")
			}

			row.SetActivatable(true)

			row.AddPrefix(activator)
			row.SetActivatableWidget(activator)

			subtitlesSelectionGroup.Add(row)
		}

		seekerIsSeeking := false
		seekerIsUnderPointer := false
		total := time.Duration(0)

		ctrl := gtk.NewEventControllerMotion()
		ctrl.ConnectEnter(func(x, y float64) {
			seekerIsUnderPointer = true
		})
		ctrl.ConnectLeave(func() {
			seekerIsUnderPointer = false
		})
		seeker.AddController(ctrl)

		seeker.ConnectChangeValue(func(scroll gtk.ScrollType, value float64) (ok bool) {
			seekerIsSeeking = true

			seeker.SetValue(value)

			elapsed := time.Duration(int64(value))

			if err := encoder.Encode(mpvCommand{[]interface{}{"seek", int64(elapsed.Seconds()), "absolute"}}); err != nil {
				openErrorDialog(ctx, window, err)

				return false
			}

			log.Info().
				Dur("duration", elapsed).
				Msg("Seeking")

			remaining := total - elapsed

			elapsedTrackLabel.SetLabel(formatDuration(elapsed))
			remainingTrackLabel.SetLabel("-" + formatDuration(remaining))

			var updateScalePosition func(done bool)
			updateScalePosition = func(done bool) {
				if seekerIsUnderPointer {
					if done {
						seekerIsSeeking = false

						return
					}

					updateScalePosition(true)
				} else {
					seekerIsSeeking = false
				}
			}

			time.AfterFunc(
				time.Millisecond*200,
				func() {
					updateScalePosition(false)
				},
			)

			return true
		})

		preparingClosed := false
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(time.Millisecond * 100)

			updateSeeker := func() {
				encoder := json.NewEncoder(sock)
				decoder := json.NewDecoder(sock)

				if err := encoder.Encode(mpvCommand{[]interface{}{"get_property", "duration"}}); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				var durationResponse mpvFloat64Response
				if err := decoder.Decode(&durationResponse); err != nil {
					log.Error().
						Err(err).
						Msg("Could not parse JSON from socket")

					return
				}

				total, err = time.ParseDuration(fmt.Sprintf("%vs", int64(durationResponse.Data)))
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				if total != 0 && !preparingClosed {
					preparingWindow.Close()

					preparingClosed = true
				}

				if err := encoder.Encode(mpvCommand{[]interface{}{"get_property", "time-pos"}}); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				var elapsedResponse mpvFloat64Response
				if err := decoder.Decode(&elapsedResponse); err != nil {
					log.Error().Err(err).Msg("Could not parse JSON from socket")

					return
				}

				elapsed, err := time.ParseDuration(fmt.Sprintf("%vs", int64(elapsedResponse.Data)))
				if err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				if !seekerIsSeeking {
					seeker.
						SetRange(0, float64(total.Nanoseconds()))
					seeker.
						SetValue(float64(elapsed.Nanoseconds()))

					remaining := total - elapsed

					log.Debug().
						Float64("total", total.Seconds()).
						Float64("elapsed", elapsed.Seconds()).
						Float64("remaining", remaining.Seconds()).
						Msg("Updating scale")

					elapsedTrackLabel.SetLabel(formatDuration(elapsed))
					remainingTrackLabel.SetLabel("-" + formatDuration(remaining))
				}
			}

			for {
				select {
				case <-t.C:
					updateSeeker()
				case <-done:
					return
				}
			}
		}()

		volumeButton.ConnectValueChanged(func(value float64) {
			log.Info().
				Float64("value", value).
				Msg("Setting volume")

			if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "volume", value * 100}}); err != nil {
				openErrorDialog(ctx, window, err)

				return
			}
		})

		subtitleButton.ConnectClicked(func() {
			subtitlesDialog.Show()
		})

		escCtrl := gtk.NewEventControllerKey()
		subtitlesDialog.AddController(escCtrl)
		subtitlesDialog.SetTransientFor(&window.Window)

		subtitlesDialog.ConnectCloseRequest(func() (ok bool) {
			subtitlesDialog.Close()
			subtitlesDialog.SetVisible(false)

			return ok
		})

		escCtrl.ConnectKeyReleased(func(keyval, keycode uint, state gdk.ModifierType) {
			if keycode == keycodeEscape {
				subtitlesDialog.Close()
				subtitlesDialog.SetVisible(false)
			}
		})

		subtitlesCancelButton.ConnectClicked(func() {
			subtitlesDialog.Close()
		})

		subtitlesOKButton.ConnectClicked(func() {
			subtitlesDialog.Close()
		})

		addSubtitlesFromFileButton.ConnectClicked(func() {
			filePicker := gtk.NewFileChooserNative(
				"Select storage location",
				&window.Window,
				gtk.FileChooserActionOpen,
				"",
				"")
			filePicker.SetModal(true)
			filePicker.ConnectResponse(func(responseId int) {
				if responseId == int(gtk.ResponseAccept) {
					log.Info().
						Str("path", filePicker.File().Path()).
						Msg("Setting subtitles")

					m := filePicker.File().Path()
					if err := encoder.Encode(mpvCommand{[]interface{}{"change-list", "sub-files", "set", m}}); err != nil {
						openErrorDialog(ctx, window, err)

						return
					}

					row := adw.NewActionRow()

					activator := gtk.NewCheckButton()

					activator.SetGroup(activators[len(activators)-1])
					activators = append(activators, activator)

					activator.SetActive(true)
					activator.ConnectActivate(func() {
						if err := encoder.Encode(mpvCommand{[]interface{}{"change-list", "sub-files", "set", m}}); err != nil {
							openErrorDialog(ctx, window, err)

							return
						}
					})

					row.SetTitle(filePicker.File().Basename())
					row.SetSubtitle("Manually added")

					row.SetActivatable(true)

					row.AddPrefix(activator)
					row.SetActivatableWidget(activator)

					subtitlesSelectionGroup.Add(row)
				}

				filePicker.Destroy()
			})

			filePicker.Show()
		})

		fullscreenButton.ConnectClicked(func() {
			if fullscreenButton.Active() {
				log.Info().Msg("Enabling fullscreen")

				if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "fullscreen", true}}); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				return
			}

			log.Info().Msg("Disabling fullscreen")

			if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "fullscreen", false}}); err != nil {
				openErrorDialog(ctx, window, err)

				return
			}
		})

		playButton.ConnectClicked(func() {
			if playButton.IconName() == playIcon {
				log.Info().Msg("Starting playback")

				playButton.SetIconName(pauseIcon)

				if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "pause", false}}); err != nil {
					openErrorDialog(ctx, window, err)

					return
				}

				return
			}

			log.Info().Msg("Pausing playback")

			if err := encoder.Encode(mpvCommand{[]interface{}{"set_property", "pause", true}}); err != nil {
				openErrorDialog(ctx, window, err)

				return
			}

			playButton.SetIconName(playIcon)
		})

		go func() {
			if err := command.Wait(); err != nil && err.Error() != errKilled.Error() {
				openErrorDialog(ctx, window, err)

				return
			}

			done <- struct{}{}

			window.Destroy()
		}()

		playButton.GrabFocus()
	})

	window.Show()

	return nil
}

func addMainMenu(ctx context.Context, app *adw.Application, window *adw.ApplicationWindow, settings *gio.Settings, menuButton *gtk.MenuButton, overlay *adw.ToastOverlay, gateway *server.Gateway, cancel func()) (*adw.PreferencesWindow, *gtk.Entry) {
	menuBuilder := gtk.NewBuilderFromString(menuUI, len(menuUI))
	menu := menuBuilder.GetObject("main-menu").Cast().(*gio.Menu)

	aboutBuilder := gtk.NewBuilderFromString(aboutUI, len(aboutUI))
	aboutDialog := aboutBuilder.GetObject("about-dialog").Cast().(*gtk.AboutDialog)

	preferencesBuilder := gtk.NewBuilderFromString(preferencesUI, len(preferencesUI))
	preferencesWindow := preferencesBuilder.GetObject("preferences-window").Cast().(*adw.PreferencesWindow)
	storageLocationInput := preferencesBuilder.GetObject("storage-location-input").Cast().(*gtk.Button)
	mpvCommandInput := preferencesBuilder.GetObject("mpv-command-input").Cast().(*gtk.Entry)
	verbosityLevelInput := preferencesBuilder.GetObject("verbosity-level-input").Cast().(*gtk.SpinButton)
	remoteGatewaySwitchInput := preferencesBuilder.GetObject("htorrent-remote-gateway-switch").Cast().(*gtk.Switch)
	remoteGatewayURLInput := preferencesBuilder.GetObject("htorrent-url-input").Cast().(*gtk.Entry)
	remoteGatewayUsernameInput := preferencesBuilder.GetObject("htorrent-username-input").Cast().(*gtk.Entry)
	remoteGatewayPasswordInput := preferencesBuilder.GetObject("htorrent-password-input").Cast().(*gtk.Entry)
	remoteGatewayURLRow := preferencesBuilder.GetObject("htorrent-url-row").Cast().(*adw.ActionRow)
	remoteGatewayUsernameRow := preferencesBuilder.GetObject("htorrent-username-row").Cast().(*adw.ActionRow)
	remoteGatewayPasswordRow := preferencesBuilder.GetObject("htorrent-password-row").Cast().(*adw.ActionRow)

	preferencesHaveChanged := false

	preferencesAction := gio.NewSimpleAction(preferencesActionName, nil)
	preferencesAction.ConnectActivate(func(parameter *glib.Variant) {
		preferencesWindow.Show()
	})
	app.SetAccelsForAction(preferencesActionName, []string{`<Primary>comma`})
	window.AddAction(preferencesAction)

	preferencesWindow.SetTransientFor(&window.Window)
	preferencesWindow.ConnectCloseRequest(func() (ok bool) {
		preferencesWindow.Close()
		preferencesWindow.SetVisible(false)

		if preferencesHaveChanged {
			settings.Apply()

			toast := adw.NewToast("Reopen to apply the changes.")
			toast.SetButtonLabel("Reopen")
			toast.SetActionName("win." + applyPreferencesActionName)

			overlay.AddToast(toast)
		}

		preferencesHaveChanged = false

		return ok
	})

	syncSensitivityState := func() {
		if remoteGatewaySwitchInput.State() {
			remoteGatewayURLRow.SetSensitive(true)
			remoteGatewayUsernameRow.SetSensitive(true)
			remoteGatewayPasswordRow.SetSensitive(true)
		} else {
			remoteGatewayURLRow.SetSensitive(false)
			remoteGatewayUsernameRow.SetSensitive(false)
			remoteGatewayPasswordRow.SetSensitive(false)
		}
	}
	preferencesWindow.ConnectShow(syncSensitivityState)

	applyPreferencesAction := gio.NewSimpleAction(applyPreferencesActionName, nil)
	applyPreferencesAction.ConnectActivate(func(parameter *glib.Variant) {
		cancel()

		if gateway != nil {
			if err := gateway.Close(); err != nil {
				openErrorDialog(ctx, window, err)

				return
			}
		}

		ex, err := os.Executable()
		if err != nil {
			openErrorDialog(ctx, window, err)

			return
		}

		if _, err := syscall.ForkExec(
			ex,
			os.Args,
			&syscall.ProcAttr{
				Env:   os.Environ(),
				Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
			},
		); err != nil {
			openErrorDialog(ctx, window, err)

			return
		}

		os.Exit(0)
	})
	window.AddAction(applyPreferencesAction)

	storageLocationInput.ConnectClicked(func() {
		filePicker := gtk.NewFileChooserNative(
			"Select storage location",
			&preferencesWindow.Window.Window,
			gtk.FileChooserActionSelectFolder,
			"",
			"")
		filePicker.SetModal(true)
		filePicker.ConnectResponse(func(responseId int) {
			if responseId == int(gtk.ResponseAccept) {
				settings.SetString(storageFlag, filePicker.File().Path())

				preferencesHaveChanged = true
			}

			filePicker.Destroy()
		})

		filePicker.Show()
	})

	settings.Bind(mpvFlag, mpvCommandInput.Object, "text", gio.SettingsBindDefault)

	verbosityLevelInput.SetAdjustment(gtk.NewAdjustment(0, 0, 8, 1, 1, 1))
	settings.Bind(verboseFlag, verbosityLevelInput.Object, "value", gio.SettingsBindDefault)

	settings.Bind(gatewayRemoteFlag, remoteGatewaySwitchInput.Object, "active", gio.SettingsBindDefault)
	settings.Bind(gatewayURLFlag, remoteGatewayURLInput.Object, "text", gio.SettingsBindDefault)
	settings.Bind(gatewayUsernameFlag, remoteGatewayUsernameInput.Object, "text", gio.SettingsBindDefault)
	settings.Bind(gatewayPasswordFlag, remoteGatewayPasswordInput.Object, "text", gio.SettingsBindDefault)

	mpvCommandInput.ConnectChanged(func() {
		preferencesHaveChanged = true
	})
	verbosityLevelInput.ConnectChanged(func() {
		preferencesHaveChanged = true
	})

	remoteGatewaySwitchInput.ConnectStateSet(func(state bool) (ok bool) {
		preferencesHaveChanged = true

		remoteGatewaySwitchInput.SetState(state)

		syncSensitivityState()

		return true
	})

	remoteGatewayURLInput.ConnectChanged(func() {
		preferencesHaveChanged = true
	})
	remoteGatewayUsernameInput.ConnectChanged(func() {
		preferencesHaveChanged = true
	})
	remoteGatewayPasswordInput.ConnectChanged(func() {
		preferencesHaveChanged = true
	})

	aboutAction := gio.NewSimpleAction("about", nil)
	aboutAction.ConnectActivate(func(parameter *glib.Variant) {
		aboutDialog.Show()
	})
	window.AddAction(aboutAction)

	aboutDialog.SetTransientFor(&window.Window)
	aboutDialog.ConnectCloseRequest(func() (ok bool) {
		aboutDialog.Close()
		aboutDialog.SetVisible(false)

		return ok
	})

	menuButton.SetMenuModel(menu)

	return preferencesWindow, mpvCommandInput
}

func openErrorDialog(ctx context.Context, window *adw.ApplicationWindow, err error) {
	errorBuilder := gtk.NewBuilderFromString(errorUI, len(errorUI))
	errorDialog := errorBuilder.GetObject("error-dialog").Cast().(*gtk.MessageDialog)
	reportErrorButton := errorBuilder.GetObject("report-error-button").Cast().(*gtk.Button)
	closeVintangleButton := errorBuilder.GetObject("close-vintangle-button").Cast().(*gtk.Button)

	errorDialog.Object.SetObjectProperty("secondary-text", err.Error())

	errorDialog.SetDefaultWidget(reportErrorButton)
	errorDialog.SetTransientFor(&window.Window)
	errorDialog.ConnectCloseRequest(func() (ok bool) {
		errorDialog.Close()
		errorDialog.SetVisible(false)

		return ok
	})

	reportErrorButton.ConnectClicked(func() {
		gtk.ShowURIFull(ctx, &window.Window, issuesURL, gdk.CURRENT_TIME, func(res gio.AsyncResulter) {
			errorDialog.Close()

			os.Exit(1)
		})
	})

	closeVintangleButton.ConnectClicked(func() {
		errorDialog.Close()

		os.Exit(1)
	})

	errorDialog.Show()
}

func main() {
	tmpDir, err := os.MkdirTemp(os.TempDir(), "vintangle-gschemas")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "gschemas.compiled"), geschemas, os.ModePerm); err != nil {
		panic(err)
	}

	if err := os.Setenv(schemaDirEnvVar, tmpDir); err != nil {
		panic(err)
	}

	settings := gio.NewSettings(stateID)

	if storage := settings.String(storageFlag); strings.TrimSpace(storage) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}

		settings.SetString(storageFlag, filepath.Join(home, "Downloads", "Vintangle"))

		settings.Apply()
	}

	settings.ConnectChanged(func(key string) {
		if key == verboseFlag {
			verbose := settings.Int64(verboseFlag)

			switch verbose {
			case 0:
				zerolog.SetGlobalLevel(zerolog.Disabled)
			case 1:
				zerolog.SetGlobalLevel(zerolog.PanicLevel)
			case 2:
				zerolog.SetGlobalLevel(zerolog.FatalLevel)
			case 3:
				zerolog.SetGlobalLevel(zerolog.ErrorLevel)
			case 4:
				zerolog.SetGlobalLevel(zerolog.WarnLevel)
			case 5:
				zerolog.SetGlobalLevel(zerolog.InfoLevel)
			case 6:
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			default:
				zerolog.SetGlobalLevel(zerolog.TraceLevel)
			}
		}
	})

	app := adw.NewApplication(appID, gio.ApplicationFlags(gio.ApplicationFlagsNone))

	prov := gtk.NewCSSProvider()
	prov.LoadFromData(styleCSS)

	var gateway *server.Gateway
	ctx, cancel := context.WithCancel(context.Background())

	app.ConnectActivate(func() {
		gtk.StyleContextAddProviderForDisplay(
			gdk.DisplayGetDefault(),
			prov,
			gtk.STYLE_PROVIDER_PRIORITY_APPLICATION,
		)

		addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
		if err != nil {
			panic(err)
		}

		port, err := freeport.GetFreePort()
		if err != nil {
			panic(err)
		}
		addr.Port = port

		rand.Seed(time.Now().UnixNano())

		if err := os.MkdirAll(settings.String(storageFlag), os.ModePerm); err != nil {
			panic(err)
		}

		apiAddr := settings.String(gatewayURLFlag)
		apiUsername := settings.String(gatewayUsernameFlag)
		apiPassword := settings.String(gatewayPasswordFlag)
		if !settings.Boolean(gatewayRemoteFlag) {
			apiUsername = randSeq(20)
			apiPassword = randSeq(20)

			gateway = server.NewGateway(
				addr.String(),
				settings.String(storageFlag),
				apiUsername,
				apiPassword,
				"",
				"",
				settings.Int64(verboseFlag) > 5,
				func(peers int, total, completed int64, path string) {
					log.Info().
						Int("peers", peers).
						Int64("total", total).
						Int64("completed", completed).
						Str("path", path).
						Msg("Streaming")
				},
				ctx,
			)

			if err := gateway.Open(); err != nil {
				panic(err)
			}

			go func() {
				log.Info().
					Str("address", addr.String()).
					Msg("Gateway listening")

				if err := gateway.Wait(); err != nil {
					panic(err)
				}
			}()

			apiAddr = "http://" + addr.String()
		}

		manager := client.NewManager(
			apiAddr,
			apiUsername,
			apiPassword,
			ctx,
		)

		if err := openAssistantWindow(ctx, app, manager, apiAddr, apiUsername, apiPassword, settings, gateway, cancel, tmpDir); err != nil {
			panic(err)
		}
	})

	app.ConnectShutdown(func() {
		cancel()

		if gateway != nil {
			if err := gateway.Close(); err != nil {
				panic(err)
			}
		}
	})

	if code := app.Run(os.Args); code > 0 {
		os.Exit(code)
	}
}
