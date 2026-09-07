// Package distribution 集中 agent 自身发行物的下载地址规则（与 xbctl 升级、
// install.sh 使用同一发行源），可通过配置覆盖 DownloadBase 以支持自建分发。
package distribution

const DefaultBase = "https://github.com/coolcrow/Xboard-Node/releases"

// ArtifactURL 返回指定版本的发行资产地址；version 为空或 "latest" 时取最新。
func ArtifactURL(artifact, version string) string {
	base := DefaultBase
	if v := overrideBase; v != "" {
		base = v
	}
	if version == "" || version == "latest" {
		return base + "/latest/download/" + artifact
	}
	return base + "/download/" + version + "/" + artifact
}

// overrideBase 由配置注入（见 SetBase），默认空 = 使用 DefaultBase。
var overrideBase string

// SetBase 覆盖发行源（进程生命周期内生效；config 加载后调用一次）。
func SetBase(b string) {
	if b != "" {
		overrideBase = b
	}
}
