// Command cnb-frontend is the standalone image entrypoint for the EXPERIMENTAL
// CNB BuildKit gateway frontend. It is a thin wrapper that runs the frontend
// logic (implemented in the importable buildkit/cnbfrontend package) via the
// BuildKit gateway protocol.
//
// The same cnbfrontend.Build function is ALSO consumed in-process by pack (via
// client.Client.Build), so no frontend image is strictly required for the pack
// integration; this command exists so the frontend can additionally be used as a
// standalone `#syntax=` frontend image.
//
// See buildkit/cnbfrontend for the implementation and the
// .kiro/specs/buildkit-native-export design.
package main

import (
	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/sirupsen/logrus"

	"github.com/buildpacks/lifecycle/buildkit/cnbfrontend"
)

func main() {
	if err := grpcclient.RunFromEnvironment(appcontext.Context(), cnbfrontend.Build); err != nil {
		logrus.Errorf("cnb-frontend fatal error: %+v", err)
		panic(err)
	}
}
