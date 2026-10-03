package toolcfg

// CurrentCodexModel reads only the top-level selection. Named profiles are
// already rejected by SwitchCodexTOML rather than guessed by the switcher.
func CurrentCodexModel(content string) string {
	lines, _ := splitLines(content)
	info := scanTOML(lines)
	for i := 0; i < firstHeader(info); i++ {
		if info[i].kind == lkKeyVal && info[i].key == "model" {
			return stringValue(lines[i])
		}
	}
	return ""
}
