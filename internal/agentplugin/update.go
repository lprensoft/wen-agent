package agentplugin

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/updater"
)

const (
	maxUpdateManifestBytes  = 64 << 10
	maxUpdateSignatureBytes = 4 << 10
	maxUpdateDownloadBytes  = 64 << 20
)

type updateDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type updateManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	ArchiveURL    string `json:"archive_url"`
	SHA256        string `json:"sha256"`
}

func newUpdateClient(lang string) *http.Client {
	return &http.Client{
		// 检查与暂存各自还有 context 超时；这里只给整次请求一个最外层上限。
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("%s", i18n.T(lang, "plugin.bundle.update.err.redirects", "更新地址的重定向次数过多"))
			}
			_, err := validateHTTPSURL(lang, req.URL.String())
			return err
		},
	}
}

func validateHTTPSURL(lang, raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if raw != trimmed || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.update.err.https", "更新来源与下载地址必须是不带用户信息或片段的 HTTPS URL：%s"), raw)
	}
	return u, nil
}

func decodeBase64(lang, raw string, size int, what string) ([]byte, error) {
	text := strings.TrimSpace(raw)
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(text)
	}
	if err != nil || len(data) != size {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.update.err.base64", "%s必须是 Base64 编码的 %d 字节数据"), what, size)
	}
	return data, nil
}

func normalizePublicKey(lang, raw string) (string, error) {
	key, err := decodeBase64(lang, raw, ed25519.PublicKeySize, i18n.T(lang, "plugin.bundle.update.public_key", "发布者公钥"))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// PublicKeyFingerprint 返回用于人工核对的公钥指纹。
func PublicKeyFingerprint(lang, raw string) (string, error) {
	key, err := decodeBase64(lang, raw, ed25519.PublicKeySize, i18n.T(lang, "plugin.bundle.update.public_key", "发布者公钥"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:16], nil
}

func (s *Store) getUpdateURL(ctx context.Context, raw string, limit int64) ([]byte, error) {
	if _, err := validateHTTPSURL(s.lang, raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	client := s.client
	if client == nil {
		client = newUpdateClient(s.lang)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.fetch", "下载更新信息失败：%w"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.status", "更新地址返回 %s：%s"), resp.Status, raw)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.too_large", "更新文件超过 %d 字节上限"), limit)
	}
	return data, nil
}

func signatureURL(lang, raw string) (string, error) {
	u, err := validateHTTPSURL(lang, raw)
	if err != nil {
		return "", err
	}
	u.Path += ".sig"
	return u.String(), nil
}

func candidateToken(manifestURL, publicKey string, manifest []byte, signature string) string {
	h := sha256.New()
	for _, part := range []string{manifestURL, publicKey, signature} {
		io.WriteString(h, part)
		h.Write([]byte{0})
	}
	h.Write(manifest)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) verifyCandidate(src *UpdateSource, manifest []byte, signature string) (*UpdateCandidate, error) {
	key, err := decodeBase64(s.lang, src.PublicKey, ed25519.PublicKeySize, i18n.T(s.lang, "plugin.bundle.update.public_key", "发布者公钥"))
	if err != nil {
		return nil, err
	}
	sig, err := decodeBase64(s.lang, signature, ed25519.SignatureSize, i18n.T(s.lang, "plugin.bundle.update.signature", "更新清单签名"))
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(ed25519.PublicKey(key), manifest, sig) {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.bad_signature", "更新清单的 Ed25519 签名验证失败"))
	}

	var m updateManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.manifest", "更新清单不是有效 JSON：%w"), err)
	}
	if m.SchemaVersion != 1 || m.Name == "" || m.Version == "" || m.ArchiveURL == "" {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.manifest_fields", "更新清单必须包含 schema_version=1、name、version 与 archive_url"))
	}
	if err := checkName(s.lang, m.Name); err != nil {
		return nil, err
	}
	if _, err := validateHTTPSURL(s.lang, m.ArchiveURL); err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(m.SHA256)
	if err != nil || len(digest) != sha256.Size || m.SHA256 != strings.ToLower(m.SHA256) {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.sha256", "更新清单的 sha256 必须是 64 位小写十六进制"))
	}
	return &UpdateCandidate{
		Name: m.Name, Version: m.Version, ArchiveURL: m.ArchiveURL, SHA256: m.SHA256,
		Manifest: append([]byte(nil), manifest...), Signature: strings.TrimSpace(signature),
		Token: candidateToken(src.ManifestURL, src.PublicKey, manifest, strings.TrimSpace(signature)),
	}, nil
}

func (s *Store) currentForUpdate(name string) (*Bundle, error) {
	current, err := Load(s.lang, s.Path(name))
	if err != nil {
		return nil, err
	}
	if current.Manifest.Name != name {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.installed_name", "安装目录 %s 中的清单包名是 %s，请先修正"), name, current.Manifest.Name)
	}
	if _, ok := updater.ParseVersion(current.Manifest.Version); !ok {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.current_version", "插件包 %s 的当前版本 %q 无法比较，不能进行更新检查"), name, current.Manifest.Version)
	}
	return current, nil
}

func (s *Store) fetchCandidate(ctx context.Context, current *Bundle, src *UpdateSource) (*UpdateCandidate, error) {
	manifest, err := s.getUpdateURL(ctx, src.ManifestURL, maxUpdateManifestBytes)
	if err != nil {
		return nil, err
	}
	sigURL, err := signatureURL(s.lang, src.ManifestURL)
	if err != nil {
		return nil, err
	}
	sig, err := s.getUpdateURL(ctx, sigURL, maxUpdateSignatureBytes)
	if err != nil {
		return nil, err
	}
	candidate, err := s.verifyCandidate(src, manifest, string(sig))
	if err != nil {
		return nil, err
	}
	if candidate.Name != current.Manifest.Name {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.name", "更新清单的包名 %q 与已安装包 %q 不一致"), candidate.Name, current.Manifest.Name)
	}
	if _, ok := updater.ParseVersion(candidate.Version); !ok {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.target_version", "候选版本 %q 无法比较"), candidate.Version)
	}
	if !updater.Newer(current.Manifest.Version, candidate.Version) {
		return nil, nil
	}
	return candidate, nil
}

// CheckUpdate 下载并验签一个包的更新清单。返回 nil candidate 表示已经是最新版。
// 它只检查，不下载 ZIP、不执行 MCP、不替换任何包文件。
func (s *Store) CheckUpdate(ctx context.Context, name string) (*UpdateCandidate, error) {
	return s.CheckUpdateAt(ctx, name, time.Now())
}

// CheckUpdateAt 与 CheckUpdate 相同，但由调用方给出检查时刻，供虚拟时钟评测使用。
func (s *Store) CheckUpdateAt(ctx context.Context, name string, checkedAt time.Time) (*UpdateCandidate, error) {
	if err := checkName(s.lang, name); err != nil {
		return nil, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if pending, err := s.Pending(name); err != nil {
		return nil, err
	} else if pending != nil {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.pending_exists", "这个包已经有待重启应用的更新"))
	}
	sources, err := s.readSources()
	if err != nil {
		return nil, err
	}
	src := sources[name]
	if src == nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.untracked", "插件包 %s 还没有绑定更新来源"), name)
	}
	current, err := s.currentForUpdate(name)
	if err != nil {
		return nil, err
	}
	candidate, checkErr := s.fetchCandidate(ctx, current, src)
	if checkErr == nil {
		src.LastCheck = checkedAt
		src.LastError = ""
		src.Candidate = candidate
	} else {
		src.LastError = checkErr.Error()
	}
	if err := s.writeSources(sources); err != nil {
		return nil, err
	}
	return candidate, checkErr
}

// downloadCandidate 把候选 ZIP 写到 dst，并在写盘的同时核对签名清单绑定的摘要。
func (s *Store) downloadCandidate(ctx context.Context, candidate *UpdateCandidate, dst string) error {
	if _, err := validateHTTPSURL(s.lang, candidate.ArchiveURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.ArchiveURL, nil)
	if err != nil {
		return err
	}
	client := s.client
	if client == nil {
		client = newUpdateClient(s.lang)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.download", "下载插件包更新失败：%w"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.download_status", "插件包下载地址返回 %s"), resp.Status)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxUpdateDownloadBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxUpdateDownloadBytes {
		return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.download_large", "插件包压缩文件超过 %d MiB 上限"), maxUpdateDownloadBytes>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != candidate.SHA256 {
		return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.digest", "插件包 SHA256 不匹配：签名清单为 %s，实际为 %s"), candidate.SHA256, got)
	}
	return nil
}
