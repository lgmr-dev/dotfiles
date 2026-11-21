package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (p *Sentinel) resolve(ctx context.Context) (SentinelRelease, error) {
	if !p.looked {
		p.looked = true
		switch {
		case p.Latest != nil:
			p.latest, p.latestFailure = p.Latest(ctx)
		case sentinelFixedRelease:
			p.latest = p.Floor
		default:
			p.latest, p.latestFailure = p.githubRelease(ctx)
		}
		if p.latestFailure == nil && (!sentinelTag.MatchString(p.latest.Tag) || !sentinelCommit(p.latest.Commit)) {
			p.latestFailure = fmt.Errorf("GitHub gave the Sentinel release %q with the commit %q", p.latest.Tag, p.latest.Commit)
		}
		if p.latestFailure == nil && sentinelOlder(p.latest.Tag, p.Floor.Tag) {
			p.latestFailure = fmt.Errorf("the latest Sentinel release %s is older than %s, the oldest release that setup installs", p.latest.Tag, p.Floor.Tag)
		}
	}
	return p.latest, p.latestFailure
}

func (p *Sentinel) githubRelease(ctx context.Context) (SentinelRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var client http.Client
	if p.Client != nil {
		client = *p.Client
	}
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = sentinelAuthorization{p.Token, next}
	get := func(url, accept string) ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", accept)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", "dotfiles-installer")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err == nil && response.StatusCode != http.StatusOK {
			err = fmt.Errorf("%s answered %s", url, response.Status)
		}
		return body, err
	}
	var release struct {
		Tag                          string `json:"tag_name"`
		Draft, Prerelease, Immutable bool
	}
	data, err := get(p.Releases, "application/vnd.github+json")
	if err == nil {
		err = json.Unmarshal(data, &release)
	}
	if err == nil && (release.Draft || release.Prerelease || !release.Immutable || !sentinelTag.MatchString(release.Tag)) {
		err = fmt.Errorf("the latest Sentinel release %q is not a published and immutable vX.Y.Z release", release.Tag)
	}
	if err != nil {
		return SentinelRelease{}, err
	}
	data, err = get(strings.TrimSuffix(p.Releases, "/releases/latest")+"/commits/refs/tags/"+release.Tag, "application/vnd.github.sha")
	return SentinelRelease{release.Tag, strings.TrimSpace(string(data))}, err
}

type sentinelAuthorization struct {
	token string
	next  http.RoundTripper
}

func (a sentinelAuthorization) RoundTrip(request *http.Request) (*http.Response, error) {
	if a.token != "" && request.URL.Scheme == "https" && request.URL.Host == "api.github.com" {
		request = request.Clone(request.Context())
		request.Header.Set("Authorization", "Bearer "+a.token)
	}
	return a.next.RoundTrip(request)
}
