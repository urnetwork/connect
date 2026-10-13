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
