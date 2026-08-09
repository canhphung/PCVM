package pcvm

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type HTTPClient struct {
	Client       *http.Client
	AllowedHosts map[string]bool
	AllowHTTP    bool
	Retries      int
}

var debianCloudRedirectHosts = csvSet("laotzu.ftp.acc.umu.se,chuangtzu.ftp.acc.umu.se")

func NewHTTPClient() *HTTPClient {
	hosts := csvSet("launchermeta.mojang.com,piston-meta.mojang.com,piston-data.mojang.com,fill.papermc.io,fill-data.papermc.io,api.purpurmc.org,ci.pufferfish.host,meta.fabricmc.net,maven.minecraftforge.net,maven.neoforged.net,ci.md-5.net,hub.spigotmc.org,repo.opencollab.dev,net-secondary.web.minecraft-services.net,www.minecraft.net,minecraft.net,minecraft.azureedge.net,api.github.com,github.com,raw.githubusercontent.com,objects.githubusercontent.com,release-assets.githubusercontent.com,pypi.org,files.pythonhosted.org,nodejs.org,python.org,www.python.org,download.oracle.com,api.adoptium.net,terraria.org,www.terraria.org,factorio.com,www.factorio.com,dl.factorio.com,steamcdn-a.akamaihd.net,github-releases.githubusercontent.com,builds.dotnet.microsoft.com,linux.multitheftauto.com,mirror.multitheftauto.com,mirror-cdn.multitheftauto.com,cloud-images.ubuntu.com,cloud.debian.org,repo.almalinux.org,download.rockylinux.org,dl-cdn.alpinelinux.org")
	h := &HTTPClient{AllowedHosts: hosts, Retries: 3}
	h.Client = &http.Client{Timeout: 45 * time.Second, CheckRedirect: h.validateRedirect}
	return h
}

func (h *HTTPClient) validateRedirect(req *http.Request, via []*http.Request) error {
	if err := h.validate(req.URL.String()); err == nil {
		return nil
	}
	if len(via) == 0 || strings.ToLower(via[0].URL.Hostname()) != "cloud.debian.org" {
		return fmt.Errorf("download host %q is not allowed", req.URL.Hostname())
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || !debianCloudRedirectHosts[strings.ToLower(req.URL.Hostname())] {
		return fmt.Errorf("Debian cloud redirect host %q is not allowed", req.URL.Hostname())
	}
	return nil
}

func (h *HTTPClient) validate(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && !(h.AllowHTTP && u.Scheme == "http")) || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("refusing non-HTTPS or credentialed URL")
	}
	if !h.AllowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("download host %q is not allowed", u.Hostname())
	}
	return nil
}

func (h *HTTPClient) request(ctx context.Context, raw string) (*http.Response, error) {
	return h.requestWithClient(ctx, raw, h.Client)
}

func (h *HTTPClient) requestWithClient(ctx context.Context, raw string, client *http.Client) (*http.Response, error) {
	if err := h.validate(raw); err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < h.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "PCVM/1")
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		if err == nil {
			last = fmt.Errorf("HTTP %d from %s", resp.StatusCode, Redact(raw))
			resp.Body.Close()
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				break
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
	return nil, last
}

func (h *HTTPClient) JSON(ctx context.Context, raw string, value any) error {
	resp, err := h.request(ctx, raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(value)
}

func (h *HTTPClient) Text(ctx context.Context, raw string, limit int64) ([]byte, error) {
	resp, err := h.request(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (h *HTTPClient) Probe(ctx context.Context, raw string) error {
	if err := h.validate(raw); err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < h.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "PCVM/1 (https://github.com/canhphung/PCVM)")
		req.Header.Set("Range", "bytes=0-0")
		resp, err := h.Client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
				return nil
			}
			last = fmt.Errorf("artifact probe returned HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
	return last
}

func (h *HTTPClient) Download(ctx context.Context, artifact Artifact, dest string) (Artifact, error) {
	downloadClient := *h.Client
	downloadClient.Timeout = 30 * time.Minute
	resp, err := h.requestWithClient(ctx, artifact.URL, &downloadClient)
	if err != nil {
		return artifact, err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return artifact, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return artifact, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	h256, h512, h1 := sha256.New(), sha512.New(), sha1.New()
	const maxDownload = int64(4 << 30)
	written, err := io.Copy(io.MultiWriter(tmp, h256, h512, h1), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		tmp.Close()
		return artifact, err
	}
	if written > maxDownload {
		tmp.Close()
		return artifact, fmt.Errorf("download exceeds 4 GiB limit")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return artifact, err
	}
	if err := tmp.Close(); err != nil {
		return artifact, err
	}
	got256, got512, got1 := hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil)), hex.EncodeToString(h1.Sum(nil))
	if artifact.SHA256 != "" && !strings.EqualFold(artifact.SHA256, got256) {
		return artifact, fmt.Errorf("SHA-256 mismatch for %s", artifact.FileName)
	}
	if artifact.SHA1 != "" && !strings.EqualFold(artifact.SHA1, got1) {
		return artifact, fmt.Errorf("SHA-1 mismatch for %s", artifact.FileName)
	}
	if artifact.SHA512 != "" && !strings.EqualFold(artifact.SHA512, got512) {
		return artifact, fmt.Errorf("SHA-512 mismatch for %s", artifact.FileName)
	}
	artifact.SHA256 = got256
	if err := os.Rename(name, dest); err != nil {
		return artifact, err
	}
	return artifact, nil
}
