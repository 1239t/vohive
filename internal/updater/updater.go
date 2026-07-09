package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/1239t/vohive/internal/global"
	"github.com/1239t/vohive/pkg/logger"
	"github.com/minio/selfupdate"
	"golang.org/x/mod/semver"
)

const (
	repoOwner = "iniwex5"
	repoName  = "vohive-release"
)

type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type UpdateInfo struct {
	HasUpdate   bool   `json:"has_update"`
	CurrentVer  string `json:"current_version"`
	LatestVer   string `json:"latest_version"`
	ReleaseNote string `json:"release_note"`
	IsDocker    bool   `json:"is_docker"`
}

// CheckUpdate 检查是否有新版本
func CheckUpdate() (*UpdateInfo, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request github api failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status: %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	currentVersion := global.Version
	if !strings.HasPrefix(currentVersion, "v") {
		currentVersion = "v" + currentVersion
	}
	latestVersion := release.TagName
	if !strings.HasPrefix(latestVersion, "v") {
		latestVersion = "v" + latestVersion
	}

	// 使用 semver 比较版本
	hasUpdate := false
	if semver.IsValid(currentVersion) && semver.IsValid(latestVersion) {
		if semver.Compare(currentVersion, latestVersion) < 0 {
			hasUpdate = true
		}
	} else {
		// 如果本地或线上不是标准 semver (比如 unknown, dev 等)，可以尝试直接不等即提示更新
		if currentVersion != latestVersion {
			hasUpdate = true
		}
	}

	isDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		isDocker = true
	}

	return &UpdateInfo{
		HasUpdate:   hasUpdate,
		CurrentVer:  currentVersion,
		LatestVer:   latestVersion,
		ReleaseNote: release.Body,
		IsDocker:    isDocker,
	}, nil
}

// ApplyUpdate 获取最新 release 并下载对应架构的二进制进行自我替换
func ApplyUpdate() error {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to fetch release info: %w", err)
	}
	defer resp.Body.Close()

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to decode release info: %w", err)
	}

	// 拼接对应的 asset name。例如: vohive_v1.0.0_linux_amd64
	targetGoos := runtime.GOOS
	targetGoarch := runtime.GOARCH
	if targetGoarch == "arm" {
		targetGoarch = "armv7" // 根据 Makefile 中的定义，vohive 编的 arm 是 armv7
	}

	binaryName := "vohive"
	assetPrefix := fmt.Sprintf("%s_%s_%s_%s", binaryName, release.TagName, targetGoos, targetGoarch)

	var downloadURL string
	for _, asset := range release.Assets {
		if strings.HasPrefix(asset.Name, assetPrefix) {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return fmt.Errorf("no matching asset found for architecture %s_%s", targetGoos, targetGoarch)
	}

	logger.Info("开始下载更新", "url", downloadURL)

	// 下载二进制
	dlResp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", dlResp.StatusCode)
	}

	// 执行替换
	err = selfupdate.Apply(dlResp.Body, selfupdate.Options{})
	if err != nil {
		// 回滚
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			return fmt.Errorf("update failed and rollback failed: %v, original error: %w", rerr, err)
		}
		return fmt.Errorf("update failed: %w", err)
	}

	logger.Info("应用更新成功，正在准备重启...")

	// 延迟退出以便接口能返回成功响应
	go func() {
		time.Sleep(2 * time.Second)
		logger.Info("进程发出关闭信号以应用更新")
		if process, err := os.FindProcess(os.Getpid()); err == nil {
			process.Signal(syscall.SIGTERM)
		} else {
			os.Exit(0)
		}
	}()

	return nil
}
