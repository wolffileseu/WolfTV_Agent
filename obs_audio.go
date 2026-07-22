package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/events"
	"github.com/andreykaipov/goobs/api/events/subscriptions"
	"github.com/andreykaipov/goobs/api/requests/inputs"
)

/* audioMonitor keeps a dedicated OBS websocket connection subscribed to
 * the (high volume) InputVolumeMeters events and watches the level of the
 * configured stream audio input (e.g. "CABLE Output"). If it stays silent
 * longer than audio_silence_sec, /status reports audio.ok=false and a
 * warning is logged -- the panel shows it. */

func audioMonitor() {
	if cfg.AudioInput == "" {
		log.Println("audio: monitor disabled (audio_input empty)")
		return
	}
	for {
		opts := []goobs.Option{
			goobs.WithEventSubscriptions(subscriptions.All | subscriptions.InputVolumeMeters),
		}
		if cfg.ObsPassword != "" {
			opts = append(opts, goobs.WithPassword(cfg.ObsPassword))
		}
		c, err := goobs.New(cfg.ObsAddr, opts...)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		st.mu.Lock()
		st.audioMonUp = true
		st.audioLastLoud = time.Now() // grace period after (re)connect
		st.mu.Unlock()
		log.Println("audio: monitor connected, watching input:", cfg.AudioInput)

		// self-diagnosis: does the configured input actually exist in OBS?
		if list, err := c.Inputs.GetInputList(inputs.NewGetInputListParams()); err == nil {
			var names []string
			found := false
			for _, in := range list.Inputs {
				b, _ := json.Marshal(in)
				var v struct {
					InputName string `json:"inputName"`
				}
				if json.Unmarshal(b, &v) == nil && v.InputName != "" {
					names = append(names, v.InputName)
					if v.InputName == cfg.AudioInput {
						found = true
					}
				}
			}
			if !found {
				log.Printf("audio: WARNING input %q not found in OBS! Available inputs: %s",
					cfg.AudioInput, strings.Join(names, " | "))
			}
		}

		wasSilent := false
		c.Listen(func(event any) {
			e, ok := event.(*events.InputVolumeMeters)
			if !ok {
				return
			}
			// tolerant parse via JSON round-trip (struct layout differs
			// slightly between goobs versions)
			b, _ := json.Marshal(e)
			var ev struct {
				Inputs []struct {
					InputName      string      `json:"inputName"`
					InputLevelsMul [][]float64 `json:"inputLevelsMul"`
				} `json:"inputs"`
			}
			if json.Unmarshal(b, &ev) != nil {
				return
			}
			for _, in := range ev.Inputs {
				if in.InputName != cfg.AudioInput {
					continue
				}
				max := 0.0
				for _, ch := range in.InputLevelsMul {
					for _, v := range ch {
						if v > max {
							max = v
						}
					}
				}
				st.mu.Lock()
				st.audioLevel = max
				if max > 0.0001 {
					st.audioLastLoud = time.Now()
				}
				silent := time.Since(st.audioLastLoud) >
					time.Duration(cfg.AudioSilenceSec)*time.Second
				st.mu.Unlock()

				if silent && !wasSilent {
					log.Printf("audio: WARNING no signal on %q for %ds",
						cfg.AudioInput, cfg.AudioSilenceSec)
					wasSilent = true
					if cfg.AudioAutoFix {
						st.mu.Lock()
						if time.Since(st.lastAudioFix) > 5*time.Minute &&
							st.pipeExecLocked("snd_restart") {
							st.lastAudioFix = time.Now()
							log.Println("audio: auto-fix sent (snd_restart)")
						}
						st.mu.Unlock()
					}
				} else if !silent && wasSilent {
					log.Println("audio: signal is back")
					wasSilent = false
				}
			}
		})

		// Listen returns when the connection dies
		st.mu.Lock()
		st.audioMonUp = false
		st.mu.Unlock()
		c.Disconnect()
		log.Println("audio: monitor disconnected, retrying in 5s")
		time.Sleep(5 * time.Second)
	}
}
