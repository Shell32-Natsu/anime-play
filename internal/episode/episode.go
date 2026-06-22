// Package episode 从动画文件名中解析集数（anitopy 风格的启发式规则，纯 Go 实现），
// 并提供目录名清洗（去字幕组标签、画质标签）用于未映射条目的搜索兜底。
package episode

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// videoExtensions 视为视频的扩展名（小写、不含点）。
var videoExtensions = map[string]bool{
	"mkv": true, "mp4": true, "ts": true, "flv": true, "webm": true,
	"m3u8": true, "avi": true, "mov": true, "wmv": true, "mpg": true,
	"mpeg": true, "m4v": true, "rmvb": true, "m2ts": true,
}

// IsVideo 判断文件名是否是视频文件。
func IsVideo(name string) bool {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	return videoExtensions[ext]
}

// 关键字标签：分辨率、来源、编码、音轨等，解析集数时要剔除，清洗目录名时也要剔除。
var keywordPattern = regexp.MustCompile(`(?i)^(` +
	`\d{3,4}[pi]|[248]k|\d{3,4}x\d{3,4}|` + // 1080p / 4K / 1920x1080
	`bd(rip|mux)?|blu-?ray|web-?(dl|rip)|dvd(rip)?|hdtv|remux|baha|bilibili|b-global|crunchyroll|cr|netflix|nf|amzn|` +
	`x26[45]|[hx]\.?26[45]|hevc|avc|av1|10-?bits?|8-?bits?|hi10p|ma10p|yuv420p10|main10|` +
	`aac(x\d)?|flac(x\d)?|ac3|eac3|ddp?(\d\.\d)?|opus|dts(-hd)?|2\.0|5\.1|` +
	`chs|cht|gb|big5|jp[nc]?|jap|sc|tc|chi|eng|简体|繁體|繁体|简繁|简日|繁日|双语|内封|内嵌|外挂|字幕|招募|` +
	`uncensored|uncut|complete|batch|fin|end|v\d|repack|rev|` +
	`mkv|mp4|avi` +
	`)$`)

func isKeywordToken(tok string) bool {
	tok = strings.Trim(tok, "[]()【】（） 　._-")
	if tok == "" {
		return true
	}
	// 复合标签如 "WEB-DL 1080p AVC AAC" 整体出现在一个括号里时按词拆开判断
	parts := strings.FieldsFunc(tok, func(r rune) bool {
		return r == ' ' || r == '_' || r == '+' || r == ',' || r == '@' || r == '&' || r == '·'
	})
	if len(parts) == 0 {
		return true
	}
	for _, p := range parts {
		if !keywordPattern.MatchString(p) {
			return false
		}
	}
	return true
}

// 各种集数表达模式，按优先级排列。
var (
	// S01E05 / s1e5
	reSeasonEp = regexp.MustCompile(`(?i)\bS\d{1,2}E(\d{1,4}(?:\.\d)?)\b`)
	// 第05話 / 第5话 / 第05集
	reCJKEp = regexp.MustCompile(`第\s*(\d{1,4}(?:\.\d)?)\s*[話话集回]`)
	// EP05 / Ep.5 / E05 / #05
	reEpPrefix = regexp.MustCompile(`(?i)(?:\bEP?\.?\s*|#)(\d{1,4}(?:\.\d)?)\b`)
	// [05] / [05v2] / 【05】
	reBracketNum = regexp.MustCompile(`[\[【]\s*(\d{1,4}(?:\.\d)?)\s*(?:v\d)?\s*[\]】]`)
	// " - 05" / "- 05v2" / "_05_" 之类用分隔符包住的纯数字
	reDashNum = regexp.MustCompile(`[\s_.\-]+(\d{1,4}(?:\.\d)?)(?:\s*v\d)?(?:[\s_.\-\[\(（【]|$)`)
)

// 看起来像年份 / 分辨率的数字不当作集数。
func plausibleEpisode(numStr string) (float64, bool) {
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, false
	}
	if n >= 1900 && n <= 2100 && n == float64(int(n)) {
		return 0, false // 年份
	}
	if n == 480 || n == 720 || n == 1080 || n == 1440 || n == 2160 {
		return 0, false // 分辨率
	}
	if n < 0 || n > 1899 {
		return 0, false
	}
	return n, true
}

// stripBrackets 去掉文件名中所有「整体是关键字标签」的括号块（保留可能含集数的括号块）。
func stripKeywordBrackets(name string) string {
	reBlock := regexp.MustCompile(`[\[【(（][^\]\[】)（）]*[\]】)）]`)
	return reBlock.ReplaceAllStringFunc(name, func(block string) string {
		if isKeywordToken(block) {
			return " "
		}
		return block
	})
}

// Parse 从文件名解析集数。返回 (集数, 是否解析成功)。
func Parse(filename string) (float64, bool) {
	base := strings.TrimSuffix(filename, path.Ext(filename))
	cleaned := stripKeywordBrackets(base)

	// 1. SxxExx
	if m := reSeasonEp.FindStringSubmatch(cleaned); m != nil {
		if n, ok := plausibleEpisode(m[1]); ok {
			return n, true
		}
	}
	// 2. 第N話
	if m := reCJKEp.FindStringSubmatch(cleaned); m != nil {
		if n, ok := plausibleEpisode(m[1]); ok {
			return n, true
		}
	}
	// 3. EP05 / E05 / #05
	if m := reEpPrefix.FindStringSubmatch(cleaned); m != nil {
		if n, ok := plausibleEpisode(m[1]); ok {
			return n, true
		}
	}
	// 4. [05]
	for _, m := range reBracketNum.FindAllStringSubmatch(cleaned, -1) {
		if n, ok := plausibleEpisode(m[1]); ok {
			return n, true
		}
	}
	// 5. " - 05" 等分隔数字，取所有候选中最像集数的（通常是最后一个非年份非分辨率数字）
	candidates := reDashNum.FindAllStringSubmatch(cleaned, -1)
	for i := len(candidates) - 1; i >= 0; i-- {
		if n, ok := plausibleEpisode(candidates[i][1]); ok {
			return n, true
		}
	}
	return 0, false
}

// FormatEpisodeNumber 把集数渲染成播放器默认识别的「第 N 话」格式。
func FormatEpisodeNumber(n float64) string {
	if n == float64(int64(n)) {
		return "第 " + strconv.FormatInt(int64(n), 10) + " 话"
	}
	return "第 " + strconv.FormatFloat(n, 'f', -1, 64) + " 话"
}

// reGenericSeason 仅表示「第几季 / SP / OVA」之类的通用文件夹名，本身不含番剧名。
var reGenericSeason = regexp.MustCompile(`(?i)^\s*(season\s*\d+|s\d{1,2}|第?\s*[一二三四五六七八九十\d]+\s*[季期部]|sps?|ova[s]?|oad[s]?|specials?|extras?|movies?|剧场版|特典|特别篇)\s*$`)

// IsGenericSeasonName 判断目录名是否只是季 / 特典之类的通用名（不含番剧名本身）。
// 这类目录（“番剧名/季/视频”结构里的季层）做搜索兜底时需要拼上父目录名。
func IsGenericSeasonName(name string) bool {
	return reGenericSeason.MatchString(name)
}

// CleanDirName 清洗目录名：去掉字幕组标签、画质/编码标签，留下大致的番剧名。
// 用于未映射条目的搜索兜底。
func CleanDirName(dirName string) string {
	s := dirName
	// 去掉所有关键字括号块
	s = stripKeywordBrackets(s)
	// 第一个括号块通常是字幕组名（如 [Lilith-Raws]），不在关键字表里也去掉
	reLeadGroup := regexp.MustCompile(`^\s*[\[【][^\]\[】]*[\]】]`)
	s = reLeadGroup.ReplaceAllString(s, " ")
	// 去掉残余的空括号和分隔符
	s = regexp.MustCompile(`[\[\]【】()（）]`).ReplaceAllString(s, " ")
	s = strings.NewReplacer("_", " ", ".", " ").Replace(s)
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return strings.TrimSpace(dirName)
	}
	return s
}
