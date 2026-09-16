package tokenizer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// o200kBaseURL is the canonical public location of the o200k_base
// mergeable-ranks payload. tiktoken-go resolves the same URL for the
// o200k_base encoding, so the cached payload is exactly what the encoder
// expects — it is not vendored into this repository.
const o200kBaseURL = "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken"

// o200kBaseFile is the cache file name inside the cache directory.
const o200kBaseFile = "o200k_base.tiktoken"

// TokenCacheDirEnv overrides the tokenizer cache directory.
const TokenCacheDirEnv = "REPOSUITE_TOKEN_CACHE_DIR"

// CacheDir returns the stable cache directory that holds the encoding
// payload. Resolution order:
//
//	$REPOSUITE_TOKEN_CACHE_DIR
//	$XDG_CACHE_HOME/reposuite-relay/tiktoken
//	$HOME/.cache/reposuite-relay/tiktoken
//	<tempdir>/reposuite-relay/tiktoken
//
// The payload is fetched once per cache location and reused by every later
// scan, so repeated gates perform no network work.
func CacheDir() string {
	if dir := strings.TrimSpace(os.Getenv(TokenCacheDirEnv)); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); dir != "" {
		return filepath.Join(dir, "reposuite-relay", "tiktoken")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "reposuite-relay", "tiktoken")
	}
	return filepath.Join(os.TempDir(), "reposuite-relay", "tiktoken")
}

// cachedLoader is the single tiktoken BPE loader used by every Counter. It
// answers only for the o200k_base payload and never touches the network
// when the payload is already cached.
type cachedLoader struct{}

func (cachedLoader) LoadTiktokenBpe(requested string) (map[string]int, error) {
	if requested != o200kBaseURL {
		return nil, fmt.Errorf("unsupported encoding payload %q", requested)
	}
	path := filepath.Join(CacheDir(), o200kBaseFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read cached %s payload: %w", EncodingName, err)
		}
		data, err = fetchPayload(path)
		if err != nil {
			return nil, err
		}
	}
	return parseRanks(data)
}

// fetchPayload downloads the payload once, into the stable cache location.
func fetchPayload(path string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(o200kBaseURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s payload (set %s to seed it offline): %w",
			EncodingName, TokenCacheDirEnv, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s payload: status %s", EncodingName, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s payload: %w", EncodingName, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create tokenizer cache directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), o200kBaseFile+".tmp")
	if err != nil {
		return nil, fmt.Errorf("create tokenizer cache file: %w", err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(tempName)
		return nil, fmt.Errorf("write tokenizer cache file: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return nil, fmt.Errorf("close tokenizer cache file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		os.Remove(tempName)
		return nil, fmt.Errorf("publish tokenizer cache file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "tokenizer: cached %s payload in %s\n", EncodingName, path)
	return data, nil
}

// parseRanks decodes the "base64-token rank" line format.
func parseRanks(data []byte) (map[string]int, error) {
	ranks := make(map[string]int)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed %s payload line", EncodingName)
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("decode %s token: %w", EncodingName, err)
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil || rank < 0 {
			return nil, fmt.Errorf("decode %s rank", EncodingName)
		}
		ranks[string(token)] = rank
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s payload: %w", EncodingName, err)
	}
	if len(ranks) == 0 {
		return nil, fmt.Errorf("%s payload is empty", EncodingName)
	}
	return ranks, nil
}
