// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package vercheck

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"text/template"
	"time"

	"ariga.io/atlas/cmd/atlas/internal/cmdstate"
)

// StateFileName is the name of the file where the vercheck state is stored.
const StateFileName = "release.json"

// timeout bounds the release lookup. The check runs on every command, so a slow
// or unreachable endpoint must not delay the CLI beyond it.
const timeout = 3 * time.Second

// New returns a new VerChecker for the endpoint. The endpoint is a GitHub
// "releases/latest" API URL, and is requested unauthenticated.
func New(endpoint string) *VerChecker {
	return &VerChecker{
		endpoint: endpoint,
		state:    &cmdstate.File[State]{Name: StateFileName},
	}
}

type (
	// Latest contains information about the most recent version.
	Latest struct {
		// Version is the new version name.
		Version string
		// Summary contains a brief description of the new version.
		Summary string
		// Link is a URL to a web page describing the new version.
		Link string
	}
	// Advisory contains contents of security advisories.
	Advisory struct {
		Text string `json:"text"`
	}
	// Payload returns information to the client about their existing version of a component.
	// The releases endpoint reports published releases only, so it never sets Advisory; the
	// field and its rendering are kept for the notification template.
	Payload struct {
		// Latest is set if there is a newer version to upgrade to.
		Latest *Latest `json:"latest"`
		// Advisory is set if security advisories exist for the current version.
		Advisory *Advisory `json:"advisory"`
	}
	// VerChecker retrieves version information from the releases endpoint.
	VerChecker struct {
		endpoint string
		state    *cmdstate.File[State]
	}
	// release is the subset of the GitHub "releases/latest" response this check reads.
	release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	// State stores information about local runs of VerChecker to limit the
	// frequency in which clients poll the service for information.
	State struct {
		CheckedAt time.Time `json:"checkedat"`
	}
)

var (
	// errSkip is returned when check isn't run because 24 hours haven't passed from the previous run.
	errSkip = errors.New("skip vercheck")
	// Notify is the template for displaying the payload to the user.
	Notify *template.Template
)

// Check makes an unauthenticated HTTP request to endpoint, the GitHub API of this
// fork's latest release, to check if a release other than the current version was
// published. Check tries to read the latest time it was run from the statePath, if
// found and 24 hours have not passed the check is skipped. When done, the latest
// time is updated in statePath.
func (v *VerChecker) Check(ctx context.Context, ver string) (*Payload, error) {
	if err := v.verifyTime(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	addHeaders(ctx, req)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if err := v.state.Write(State{CheckedAt: time.Now()}); err != nil {
		return nil, err
	}
	return r.payload(ver), nil
}

// payload reports the release to the user if it is not the running version. The fork
// tags (e.g. "v1.3.0-views-ce.3") are not ordered by semver - their suffix parses as a
// pre-release, which sorts before the release it is built on - so the comparison is an
// inequality, not a "greater than": any tag other than the one running is the one to
// upgrade to. Callers must only check versions built from this fork; see checkForUpdate.
func (r *release) payload(ver string) *Payload {
	if r.TagName == "" || r.TagName == ver {
		return &Payload{}
	}
	return &Payload{Latest: &Latest{Version: r.TagName, Link: r.HTMLURL}}
}

func (v *VerChecker) verifyTime() error {
	s, err := v.state.Read()
	if err != nil || time.Since(s.CheckedAt) >= (time.Hour*24) {
		return nil
	}
	return errSkip
}

//go:embed notification.tmpl
var notifyTmpl string

func init() {
	var err error
	Notify, err = template.New("").Parse(notifyTmpl)
	if err != nil {
		panic(err)
	}
}
