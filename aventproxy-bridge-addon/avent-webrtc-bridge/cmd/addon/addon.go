package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/rtsp"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
	"avent-webrtc-bridge/pkg/utils"
)

// configPollInterval is how often the bridge notices that the integration has
// rewritten its config. The file lives on the shared /config mount and is
// replaced rather than edited, so a stat is enough and inotify would only add
// a dependency.
const configPollInterval = 5 * time.Second

// BridgeConfig is the JSON shape written by the HA integration
// in custom_components/philips_avent/__init__.py::_write_bridge_config.
type BridgeConfig struct {
	SigningKey  string   `json:"signing_key"`
	SID         string   `json:"sid"`
	Ecode       string   `json:"ecode"`
	Partner     string   `json:"partner"`
	AppKey      string   `json:"app_key"`
	DeviceID    string   `json:"device_id"`
	PackageName string   `json:"package_name"`
	APIHost     string   `json:"api_host"`
	UID         string   `json:"uid"`
	Talkback    bool     `json:"talkback"`
	BridgePort  int      `json:"bridge_port"`
	Cameras     []Camera `json:"cameras"`
}

// Camera is one entry under "cameras" in the JSON.
type Camera struct {
	ID        string `json:"camera_id"`
	Name      string `json:"camera_name"`
	ProductID string `json:"product_id"`
	// LocalKey and Password drive the LAN path: the monitor's P2P login is
	// md5(Password + "||" + LocalKey). Empty for entries written by an older
	// integration, which simply means cloud-only for that camera.
	LocalKey string `json:"local_key"`
	Password string `json:"password"`
	// LanIP is Home Assistant's own discovery result, reused so the bridge does
	// not contend with it for the broadcast socket.
	LanIP string `json:"lan_ip"`
}

// lanUID prefers the account id the integration passed. Falling back to the
// cloud profile keeps configs written by an older integration working.
func lanUID(cfg BridgeConfig, userInfo *tuya.UserInfoResult) string {
	if cfg.UID != "" {
		return cfg.UID
	}
	if userInfo != nil {
		return userInfo.ID
	}
	return ""
}

// verifyCloud reports whether the account is usable and who it belongs to.
// Only the pair of calls proves it: the first that the host answers us, the
// second that the session is still valid.
func verifyCloud(client *tuya.MobileSDKClient) (*tuya.UserInfoResult, error) {
	core.Logger.Info().Msg("Verifying API access...")
	if _, err := client.Call("smartlife.p.time.get", "1.0", nil); err != nil {
		return nil, fmt.Errorf("api unreachable: %w", err)
	}

	userInfo, err := client.GetUserInfo()
	if err != nil {
		return nil, fmt.Errorf("session unusable: %w", err)
	}

	core.Logger.Info().Msgf("User: %s (%s)", userInfo.Nickname, utils.MaskEmail(userInfo.Email))
	client.UID = userInfo.ID
	return userInfo, nil
}

// addonIdentity names the stored session. Online that is the account's email;
// with no cloud session there is none, so the account uid the integration
// already passes stands in. It only has to be stable across restarts, because
// the cameras are stored against it and are unreachable without the match.
func addonIdentity(cfg BridgeConfig, userInfo *tuya.UserInfoResult) string {
	if userInfo != nil && userInfo.Email != "" {
		return userInfo.Email
	}
	return lanUID(cfg, nil)
}

func loadConfig(path string) (BridgeConfig, error) {
	var cfg BridgeConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// CameraWithPath pairs a Camera with the RTSP path it will be served on.
type CameraWithPath struct {
	Camera
	Path string
}

// assignPaths sanitizes each camera's name into an RTSP path and filters out
// invalid entries. Behavior:
//   - skip entries with empty camera_id (warn);
//   - skip later entries that repeat a previously-seen camera_id (warn) — a single
//     physical device must not be registered twice;
//   - when two distinct cameras sanitize to the same path, the second one gets
//     a suffix derived from up to 6 characters of its camera_id (warn).
func assignPaths(cams []Camera) []CameraWithPath {
	out := make([]CameraWithPath, 0, len(cams))
	seenIDs := make(map[string]bool, len(cams))
	seenPaths := make(map[string]bool, len(cams))
	for _, cam := range cams {
		if cam.ID == "" {
			core.Logger.Warn().Msgf("Camera config invalid: missing camera_id, skipping name=%q", cam.Name)
			continue
		}
		if seenIDs[cam.ID] {
			core.Logger.Warn().Msgf("Duplicate camera_id %q, skipping repeated entry name=%q", cam.ID, cam.Name)
			continue
		}
		seenIDs[cam.ID] = true

		basePath := storage.SanitizeRTSPPath(cam.Name, cam.ID)
		path := basePath
		if seenPaths[path] {
			suffix := cam.ID
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			path = basePath + "_" + suffix
			i := 2
			for seenPaths[path] {
				path = fmt.Sprintf("%s_%s_%d", basePath, suffix, i)
				i++
			}
			core.Logger.Warn().Msgf("Path collision on %s, falling back to %s", basePath, path)
		}
		seenPaths[path] = true
		out = append(out, CameraWithPath{Camera: cam, Path: path})
	}
	return out
}

func validateConfig(cfg BridgeConfig) error {
	if cfg.SigningKey == "" {
		return fmt.Errorf("signing_key is required")
	}
	if cfg.SID == "" {
		return fmt.Errorf("sid is required")
	}
	if cfg.AppKey == "" {
		return fmt.Errorf("app_key is required")
	}
	if cfg.DeviceID == "" {
		return fmt.Errorf("device_id is required")
	}
	if cfg.Ecode == "" {
		return fmt.Errorf("ecode is required")
	}
	if cfg.Partner == "" {
		return fmt.Errorf("partner is required")
	}
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("cameras list is empty: nothing to serve")
	}
	return nil
}

var storageManager *storage.StorageManager

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

func NewAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Run the multi-camera bridge driven by the HA integration JSON",
		Long: `Read the bridge config JSON written by the Philips Avent HA integration
and serve every camera under it from one RTSP server, each on its own path.

Example:
  avent-webrtc-bridge addon --config /config/philips_avent_bridge_<entry_id>.json`,
		RunE: runAddon,
	}
	cmd.Flags().String("config", "", "Path to the bridge config JSON written by the HA integration")
	cmd.MarkFlagRequired("config")
	return cmd
}

func runAddon(cmd *cobra.Command, args []string) error {
	cfgPath, _ := cmd.Flags().GetString("config")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	for {
		restart, err := serveOnce(cfgPath, sig)
		if err != nil || !restart {
			return err
		}
		core.Logger.Info().Msg("Bridge config changed, restarting the stream server")
	}
}

// serveOnce serves every camera in the config until a shutdown signal arrives
// or the config file changes, and reports whether the caller should start over.
func serveOnce(cfgPath string, sig <-chan os.Signal) (bool, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return false, err
	}
	if err := validateConfig(cfg); err != nil {
		return false, fmt.Errorf("invalid config %s: %w", cfgPath, err)
	}

	port := cfg.BridgePort
	if port == 0 {
		port = 38554
	}

	apiHost := tuya.NormalizeAPIHost(cfg.APIHost)
	client := tuya.NewMobileSDKClient(cfg.SigningKey, cfg.SID, cfg.AppKey, cfg.DeviceID, "071d81fa")
	client.BaseURL = tuya.APIBaseURL(apiHost)
	client.Ecode = cfg.Ecode
	client.PartnerIdentity = cfg.Partner
	client.PackageName = cfg.PackageName

	core.Logger.Info().Msgf("Tuya API host: %s", apiHost)

	// The cloud is the fallback, not the prerequisite. A monitor on the local
	// network needs none of it, so an unreachable account leaves the add-on
	// serving what it can instead of refusing to start.
	userInfo, cloudErr := verifyCloud(client)
	if cloudErr != nil {
		core.Logger.Warn().Err(cloudErr).Msg("No cloud session; the cloud path is unavailable until the add-on restarts")
	}

	identity := addonIdentity(cfg, userInfo)
	if identity == "" {
		return false, fmt.Errorf("no cloud session and no uid in the config: nothing names this account")
	}
	userKey := "addon_" + strings.ReplaceAll(strings.ReplaceAll(identity, "@", "_at_"), ".", "_")

	session := &tuya.SessionData{
		LoginResult: &tuya.LoginResult{Uid: lanUID(cfg, userInfo)},
		ServerHost:  apiHost,
		Region:      "addon",
	}
	if userInfo != nil {
		session.LoginResult.Email = userInfo.Email
		session.LoginResult.Nickname = userInfo.Nickname
		session.LoginResult.Domain = userInfo.Domain
		session.UserEmail = userInfo.Email
	}
	if err := storageManager.SaveUser("addon", identity, session); err != nil {
		core.Logger.Warn().Msgf("Could not save user session: %v", err)
	}

	camsWithPath := assignPaths(cfg.Cameras)
	if len(camsWithPath) == 0 {
		return false, fmt.Errorf("no valid cameras after filtering, refusing to start")
	}

	infos := make([]storage.CameraInfo, 0, len(camsWithPath))
	pathLog := make([]string, 0, len(camsWithPath))
	localCapable := 0
	for _, c := range camsWithPath {
		infos = append(infos, storage.CameraInfo{
			DeviceID:   c.ID,
			DeviceName: c.Name,
			Category:   "sp",
			ProductID:  c.ProductID,
			RTSPPath:   c.Path,
			UserKey:    userKey,
			LocalKey:   c.LocalKey,
			Password:   c.Password, LanIP: c.LanIP, UID: lanUID(cfg, userInfo),
		})
		pathLog = append(pathLog, c.Path)

		local := c.LocalKey != "" && c.Password != ""
		if local {
			localCapable++
		}
		core.Logger.Info().Msgf("Camera registered: id=%s name=%s path=%s local=%t", c.ID, c.Name, c.Path, local)
	}

	if cloudErr != nil && localCapable == 0 {
		return false, fmt.Errorf("no cloud session and no camera has a local key and password: nothing can be served")
	}
	if err := storageManager.UpdateCamerasForUser(userKey, infos); err != nil {
		core.Logger.Warn().Msgf("Could not save cameras: %v", err)
	}

	server := rtsp.NewRTSPServer(port, storageManager)
	if cloudErr == nil {
		server.MobileClient = client
	}
	server.Talkback = cfg.Talkback
	if cfg.Talkback {
		core.Logger.Info().Msg("Two-way audio enabled: streams will ask the camera for talkback")
	}
	if err := server.Start(); err != nil {
		return false, fmt.Errorf("start RTSP server: %w", err)
	}
	paths := "local network first, cloud fallback"
	if cloudErr != nil {
		paths = "local network only"
	}
	core.Logger.Info().Msgf("Serving %d cameras on port %d, %d of them locally (%s): %s",
		len(infos), port, localCapable, paths, strings.Join(pathLog, " "))

	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()

	var restart bool
	select {
	case <-sig:
	case <-watchConfigFile(watchCtx, cfgPath):
		restart = true
	}

	core.Logger.Info().Msg("Shutting down...")
	server.Stop()
	return restart, nil
}

// watchConfigFile closes the returned channel once the config file on disk is
// no longer the one that was loaded.
func watchConfigFile(ctx context.Context, path string) <-chan struct{} {
	changed := make(chan struct{})

	go func() {
		defer close(changed)

		loaded, err := os.Stat(path)
		if err != nil {
			<-ctx.Done()
			return
		}

		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now, err := os.Stat(path)
				if err != nil {
					continue
				}
				if !now.ModTime().Equal(loaded.ModTime()) || now.Size() != loaded.Size() {
					return
				}
			}
		}
	}()

	return changed
}
