package update

import (
	"github.com/blang/semver"
	"github.com/rhysd/go-github-selfupdate/selfupdate"
	cli "github.com/urfave/cli/v2"
	"golang.org/x/exp/slog"
)

// Handler replaces the current running binary with the latest version from github
func Handler(c *cli.Context) error {
	v := semver.MustParse(c.App.Version)
	latest, err := selfupdate.UpdateSelf(v, "mintel/amz-ssh")
	if err != nil {
		slog.Error("Binary update failed", "err", err)
		return nil
	}
	if latest.Version.Equals(v) {
		// latest version is the same as current version. It means current binary is up to date.
		slog.Info("Current binary is the latest version", "version", c.App.Version)
	} else {
		slog.Info("Successfully updated to version", "version", latest.Version)
		slog.Info("Release note:\n" + latest.ReleaseNotes)
	}

	return nil
}
