// Advisory client presentation metadata is parsed once at the transport boundary.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"
)

const ClientInfoHeader = "X-UR-ClientInfo"
const ClientInfoMaxBytes = 512

type ClientInfo struct {
	Version    int    `json:"v"`
	DeviceType string `json:"device_type"`
	AppVersion string `json:"app_version"`
	SdkVersion string `json:"sdk_version,omitempty"`
	// how the build reached the device (ClientDistribution*); "" is unknown,
	// and is left out of the json, so a client that does not know its
	// distribution sends what every client sent before the field
	Distribution string `json:"distribution,omitempty"`
}

// The distribution channels a build reports in ClientInfo.Distribution: how
// the build reached the device, and with it who bills the build's purchases, a
// store or Stripe. Each build stamps its own at build time, from its Android
// flavor, Apple's DIRECT_DOWNLOAD flag, its Linux package or its browser
// target. "" is unknown, as for every client from before the field.
//
// Each value pairs with the device types clientDistributionPairs names.
// ParseClientInfo reads a value it does not know, or one that does not pair
// with the device type, as unknown and keeps the rest of the object, so a
// newer build's value never costs an older parser the device type.
const (
	// the App Store builds for iOS, iPadOS and macOS, their beta builds
	// included: the App Store bills
	ClientDistributionAppStore = "app_store"
	// the direct-download macOS build (the Developer ID DMG with the system
	// extension tunnel, DIRECT_DOWNLOAD): Stripe bills
	ClientDistributionMacDirect = "mac_direct"
	// the Android play flavor: Google Play bills
	ClientDistributionPlay = "play"
	// the Android github flavor, without Google Play services: the APK of the
	// GitHub releases and the F-Droid build. Stripe bills
	ClientDistributionAndroidDirect = "android_direct"
	// the Android solana_dapp flavor, for the Solana dApp Store: Stripe bills
	ClientDistributionSolanaDapp = "solana_dapp"
	// the Android ethos_dapp flavor, for the Ethos dApp store: Stripe bills
	ClientDistributionEthosDapp = "ethos_dapp"
	// the Windows app; its MSI and its portable zip carry the same build.
	// Stripe bills
	ClientDistributionWindows = "windows"
	// a Linux build whose package is not known: the daemon, one build of
	// which goes into the .deb, the .rpm, the Arch package and the install
	// tarball, and a build from source. Stripe bills
	ClientDistributionLinux = "linux"
	// the Linux app's AppImage
	ClientDistributionLinuxAppimage = "linux_appimage"
	// the Linux app's Flatpak
	ClientDistributionLinuxFlatpak = "linux_flatpak"
	// ur.io in a browser: Stripe bills
	ClientDistributionWeb = "web"
	// the browser extension's Chrome build (the Chrome Web Store, and the
	// other Chromium browsers' stores)
	ClientDistributionExtensionChrome = "extension_chrome"
	// the browser extension's Firefox build (Firefox Add-ons)
	ClientDistributionExtensionFirefox = "extension_firefox"
)

// clientDistributionPairs reports whether a build of the device type can
// come through the distribution. False for "" and for any value not listed.
func clientDistributionPairs(distribution string, deviceType string) bool {
	switch distribution {
	case ClientDistributionAppStore:
		return deviceType == "ios" || deviceType == "macos"
	case ClientDistributionMacDirect:
		return deviceType == "macos"
	case ClientDistributionPlay, ClientDistributionAndroidDirect, ClientDistributionSolanaDapp, ClientDistributionEthosDapp:
		return deviceType == "android"
	case ClientDistributionWindows:
		return deviceType == "windows"
	case ClientDistributionLinux, ClientDistributionLinuxAppimage, ClientDistributionLinuxFlatpak:
		return deviceType == "linux"
	case ClientDistributionWeb, ClientDistributionExtensionChrome, ClientDistributionExtensionFirefox:
		return deviceType == "web"
	}
	return false
}

type SessionLastUsed struct {
	UnixTime    int64  `json:"unix_time"`
	City        string `json:"city"`
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	DeviceType  string `json:"device_type"`
	AppVersion  string `json:"app_version"`
}

func UnknownClientInfo() ClientInfo { return ClientInfo{Version: 1, DeviceType: "unknown"} }

func boundedClientVersion(value string) bool { return len(value) <= 64 && utf8.ValidString(value) }

// Duplicate fields, even unknown fields, invalidate the complete object. The
// fallback is the bounded legacy app version, never a different auth generation.
// A distribution that is not a string invalidates the object as any other
// mistyped field does; a string value that is unknown or does not pair with the
// device type is only dropped (clientDistributionPairs).
func ParseClientInfo(encoded, legacyAppVersion string) ClientInfo {
	fallback := UnknownClientInfo()
	if boundedClientVersion(legacyAppVersion) {
		fallback.AppVersion = legacyAppVersion
	}
	if len(encoded) == 0 || len(encoded) > ClientInfoMaxBytes || !utf8.ValidString(encoded) {
		return fallback
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return fallback
	}
	seen := map[string]bool{}
	var info ClientInfo
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return fallback
		}
		name, ok := field.(string)
		if !ok || seen[name] {
			return fallback
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return fallback
		}
		var fieldErr error
		switch name {
		case "v":
			fieldErr = json.Unmarshal(value, &info.Version)
		case "device_type":
			fieldErr = json.Unmarshal(value, &info.DeviceType)
		case "app_version":
			fieldErr = json.Unmarshal(value, &info.AppVersion)
		case "sdk_version":
			fieldErr = json.Unmarshal(value, &info.SdkVersion)
		case "distribution":
			fieldErr = json.Unmarshal(value, &info.Distribution)
		}
		if fieldErr != nil {
			return fallback
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fallback
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fallback
	}
	if info.Version != 1 || !boundedClientVersion(info.AppVersion) || !boundedClientVersion(info.SdkVersion) {
		return fallback
	}
	switch info.DeviceType {
	case "android", "ios", "macos", "windows", "linux", "web", "cli", "server", "unknown":
	default:
		return fallback
	}
	if !clientDistributionPairs(info.Distribution, info.DeviceType) {
		info.Distribution = ""
	}
	if info.AppVersion == "" {
		info.AppVersion = fallback.AppVersion
	}
	return info
}

func ClientInfoFromHeader(header http.Header) ClientInfo {
	legacy := ""
	if values := header.Values("X-UR-AppVersion"); len(values) == 1 {
		legacy = values[0]
	}
	if values := header.Values(ClientInfoHeader); len(values) == 1 {
		return ParseClientInfo(values[0], legacy)
	}
	return ParseClientInfo("", legacy)
}

func (self ClientInfo) Json() string {
	if self.Version == 0 {
		self.Version = 1
	}
	if self.DeviceType == "" {
		self.DeviceType = "unknown"
	}
	encoded, err := json.Marshal(self)
	if err != nil {
		return ""
	}
	return string(encoded)
}

type clientInfoContextKey struct{}

func WithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	return context.WithValue(ctx, clientInfoContextKey{}, info)
}
func ClientInfoFromContext(ctx context.Context) ClientInfo {
	if info, ok := ctx.Value(clientInfoContextKey{}).(ClientInfo); ok {
		return info
	}
	return UnknownClientInfo()
}
func setClientInfoHeader(ctx context.Context, header http.Header) {
	header.Set(ClientInfoHeader, ClientInfoFromContext(ctx).Json())
}
