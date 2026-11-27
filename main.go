// Command plugin-json is the JSON-file backend plugin of Spec Studio. It
// keeps a host's documents and blobs as files in one data folder. See
// README.md and https://github.com/spec-studio-app/plugin-sdk.
package main

import (
	"github.com/spec-studio-app/plugin-json/internal/store"
	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// version is set by the release build: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

// Repo is the plugin's repo link, shown by the hosts.
const Repo = "https://github.com/spec-studio-app/plugin-json"

func newPlugin() plugin.Plugin {
	return plugin.Plugin{
		Info: protocol.PluginInfo{Name: "json", Version: version, Repo: Repo},
		Settings: []protocol.Setting{{
			Name: store.SettingDataDir,
			Description: "The data folder. With no value: /data for the relay, " +
				"<support folder>/backend/json for the app, and a new temporary folder for a test host.",
		}},
		Store: store.New(),
	}
}

func main() {
	plugin.Main(newPlugin())
}
