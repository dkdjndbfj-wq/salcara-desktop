package agents

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"salcara/bridge/internal/protocol"
)

const skillIconPrefix = "data:image/png;base64,"

var skillBrandColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validSkillDisplayName(value string) bool {
	if value == "" {
		return true
	}
	return len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// Only a bounded, static raster crosses the device boundary. Never fetch URLs
// or interpret SVG/HTML supplied by a catalog or a phone message.
func validSkillIcon(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 5486 || !strings.HasPrefix(value, skillIconPrefix) {
		return false
	}
	data, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(value, skillIconPrefix))
	if err != nil || len(data) > 4096 {
		return false
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Width > 32 || config.Height < 1 || config.Height > 32 {
		return false
	}
	_, err = png.Decode(bytes.NewReader(data))
	return err == nil
}

func validSkillDisplayMetadata(skill protocol.SkillReference) bool {
	return validSkillDisplayName(skill.DisplayName) && (skill.BrandColor == "" || skillBrandColor.MatchString(skill.BrandColor)) && validSkillIcon(skill.IconSmall)
}

func skillReferenceWithDisplay(skill protocol.Skill) protocol.SkillReference {
	return protocol.SkillReference{Name: skill.Name, Path: skill.Path, DisplayName: skill.DisplayName, BrandColor: skill.BrandColor, IconSmall: skill.IconSmall}
}

// skillPath must come from the enabled native catalog. A phone-supplied path
// never reaches this function. Restrict icon resources to that skill directory,
// resolve symlinks, verify the opened regular file, then decode/reencode PNG.
func readSkillCatalogIcon(skillPath, iconPath string) string {
	if iconPath == "" || len(iconPath) > 4096 || strings.ContainsAny(iconPath, "\x00\r\n") || strings.Contains(iconPath, "://") {
		return ""
	}
	resolvedSkill, err := filepath.EvalSymlinks(skillPath)
	if err != nil {
		return ""
	}
	root := filepath.Dir(resolvedSkill)
	candidate := iconPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return ""
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return ""
	}
	file, err := os.Open(resolved)
	if err != nil {
		return ""
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return ""
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 512 || config.Height > 512 {
		return ""
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	width, height := config.Width, config.Height
	if width > 32 || height > 32 {
		if width >= height {
			height = max(1, height*32/width)
			width = 32
		} else {
			width = max(1, width*32/height)
			height = 32
		}
	}
	normalized := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			normalized.Set(x, y, decoded.At(x*config.Width/width, y*config.Height/height))
		}
	}
	var output bytes.Buffer
	if png.Encode(&output, normalized) != nil || output.Len() > 4096 {
		return ""
	}
	return skillIconPrefix + base64.StdEncoding.EncodeToString(output.Bytes())
}
