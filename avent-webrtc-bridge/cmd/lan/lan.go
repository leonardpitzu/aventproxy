// Package lan provides a diagnostic for the cloudless streaming path.
package lan

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/storage"
)

var storageManager *storage.StorageManager

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

// NewLanCmd builds the diagnostic. The LAN path can only be verified against a
// real monitor, so this exists to do that without Home Assistant in the way.
func NewLanCmd() *cobra.Command {
	var (
		deviceID string
		ip       string
		localKey string
		password string
		uid      string
		seconds  int
		verbose  bool
	)

	cmd := &cobra.Command{
		Use:   "lan",
		Short: "Stream from a monitor over the local network, without the cloud",
		Long: `Connect to a monitor on the LAN and report what happens at every step:
session negotiation, the protocol-302 offer, ICE, then the video itself.

Credentials come from the camera's entry in the bridge config, or from flags.
The monitor must have reached the Tuya cloud once since it last booted, or it
ignores local offers entirely.

Example:
  avent-webrtc-bridge lan --camera-id bfc4... --ip 192.168.0.15 \
    --local-key 'xxxxxxxxxxxxxxxx' --password xxxxxxxx`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if deviceID == "" || localKey == "" || password == "" {
				return fmt.Errorf("--camera-id, --local-key and --password are required")
			}
			if verbose {
				lan.Debugf = func(format string, args ...any) {
					fmt.Printf("  %s\n", fmt.Sprintf(format, args...))
				}
			}

			var frames, bytes int
			var width, height, fps int
			client := lan.NewClient(deviceID, localKey, password, uid, ip, func(f *lan.VideoFrame) {
				frames++
				bytes += len(f.NAL)
				// Only the fragment that opens a picture carries the stream
				// description; the rest hold whatever was in those bytes.
				if width == 0 && f.Width >= 160 && f.Height >= 120 && f.FPS > 0 {
					width, height, fps = f.Width, f.Height, f.FPS
				}
			})

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+45)*time.Second)
			defer cancel()

			fmt.Printf("connecting to %s ...\n", deviceID)
			start := time.Now()
			if err := client.Start(ctx); err != nil {
				return fmt.Errorf("local streaming unavailable: %w", err)
			}
			defer client.Close()
			fmt.Printf("connected in %.1fs, collecting for %ds\n", time.Since(start).Seconds(), seconds)

			time.Sleep(time.Duration(seconds) * time.Second)

			if frames == 0 {
				return fmt.Errorf("connected but no video arrived")
			}
			fmt.Printf("\n%dx%d at %dfps: %d frames, %d bytes in %ds\n",
				width, height, fps, frames, bytes, seconds)
			return nil
		},
	}

	cmd.Flags().StringVar(&deviceID, "camera-id", "", "camera device id")
	cmd.Flags().StringVar(&ip, "ip", "", "monitor address; discovered by broadcast when empty")
	cmd.Flags().StringVar(&localKey, "local-key", "", "device localKey")
	cmd.Flags().StringVar(&password, "password", "", "device P2P password, from the RTC config")
	cmd.Flags().StringVar(&uid, "uid", "", "account uid, used as the offer's sender")
	cmd.Flags().IntVar(&seconds, "seconds", 10, "how long to collect video")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "trace the protocol")
	return cmd
}
