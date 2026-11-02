package steps

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// structureIntent rewrites the published Intent into scannable points: a short
// statement of the goal, then one bullet per discrete ask or constraint.
//
// It exists because the intent is the driving agent's own multi-paragraph brief
// and reached a Bitbucket description verbatim, as a wall of prose that states
// the same point more than once. It is deterministic on purpose. The end-of-run
// refresh rebuilds the description without any agent turn, from the raw intent,
// so an agent-written summary would be lost there (or would need a stored
// copy), and a rewrite by a model can silently drop a requirement. Everything
// this does can be checked: it only splits, re-lists, code-formats, and removes
// a statement that another bullet already contains in full.
//
// What it keeps, by construction:
//   - every distinct statement, in the order it appears (only an exact or
//     fully-contained repeat is removed, and never one that differs in a number,
//     identifier or negation);
//   - fenced code, tables and ordered lists, verbatim and in order;
//   - the intent's own bullets, as bullets.
//
// A short intent (at most two sentences, no structure) is only code-formatted.
func structureIntent(intent string) string {
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return ""
	}
	blocks := parseIntentBlocks(intent)
	if intentIsShort(blocks) {
		return codeFormatIntent(intent)
	}

	var out intentOutput
	first := true
	for i, blk := range blocks {
		switch blk.kind {
		case intentVerbatim:
			out.verbatim(blk.text)
		case intentLabel:
			out.label(blk.text)
		case intentOrdered:
			out.ordered(blk.items)
		case intentBullets:
			for _, item := range blk.items {
				out.bullets(splitIntentLong(item))
			}
		case intentParagraph:
			sentences := splitIntentSentences(blk.text)
			if first {
				sentences = out.goal(sentences)
			}
			lead := ""
			if n := len(sentences); n > 0 && strings.HasSuffix(sentences[n-1], ":") && followsStructure(blocks, i) {
				lead, sentences = sentences[n-1], sentences[:n-1]
			}
			var points []string
			for _, s := range mergeShortSentences(sentences) {
				points = append(points, splitIntentLong(s)...)
			}
			out.bullets(points)
			if lead != "" {
				out.lead(lead)
			}
		}
		first = false
	}
	return strings.TrimSpace(out.render())
}

const (
	// intentLineCap is the length past which a sentence is split into separate
	// points.
	intentLineCap = 220
	// intentGoalMax bounds the goal statement.
	intentGoalMax = 280
)

type intentKind int

const (
	intentParagraph intentKind = iota
	intentLabel
	intentBullets
	intentOrdered
	intentVerbatim
)

type intentBlock struct {
	kind  intentKind
	text  string
	items []string
}

var (
	intentFenceRE   = regexp.MustCompile("^ {0,3}(```|~~~)")
	intentHeadingRE = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	intentBulletRE  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	intentOrderedRE = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
)

// parseIntentBlocks splits the intent into paragraphs, labels (headings and
// short "Label:" lines), bullet and ordered lists, and verbatim blocks (fenced
// code and tables).
func parseIntentBlocks(intent string) []intentBlock {
	var blocks []intentBlock
	var para []string
	var items []string
	itemKind := intentKind(-1)
	var fence []string
	inFence := false
	var table []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		text := strings.Join(para, " ")
		lines := para
		para = nil
		if looksLikeCode(lines) {
			blocks = append(blocks, intentBlock{kind: intentVerbatim, text: "```text\n" + strings.Join(lines, "\n") + "\n```"})
			return
		}
		if isIntentLabel(text) {
			blocks = append(blocks, intentBlock{kind: intentLabel, text: text})
			return
		}
		blocks = append(blocks, intentBlock{kind: intentParagraph, text: text})
	}
	flushItems := func() {
		if len(items) == 0 {
			return
		}
		blocks = append(blocks, intentBlock{kind: itemKind, items: items})
		items = nil
		itemKind = -1
	}
	flushTable := func() {
		if len(table) == 0 {
			return
		}
		blocks = append(blocks, intentBlock{kind: intentVerbatim, text: strings.Join(table, "\n")})
		table = nil
	}
	flushAll := func() {
		flushPara()
		flushItems()
		flushTable()
	}

	for _, raw := range strings.Split(strings.ReplaceAll(intent, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		if inFence {
			fence = append(fence, strings.TrimLeft(line, " "))
			if intentFenceRE.MatchString(line) {
				inFence = false
				blocks = append(blocks, intentBlock{kind: intentVerbatim, text: strings.Join(fence, "\n")})
				fence = nil
			}
			continue
		}
		switch {
		case intentFenceRE.MatchString(line):
			flushAll()
			inFence = true
			fence = []string{strings.TrimLeft(line, " ")}
		case strings.TrimSpace(line) == "":
			flushAll()
		case strings.HasPrefix(strings.TrimSpace(line), "|"):
			flushPara()
			flushItems()
			table = append(table, strings.TrimSpace(line))
		case intentHeadingRE.MatchString(line):
			flushAll()
			blocks = append(blocks, intentBlock{kind: intentLabel, text: intentHeadingRE.FindStringSubmatch(line)[1]})
		case intentBulletRE.MatchString(line):
			flushPara()
			flushTable()
			if itemKind != intentBullets {
				flushItems()
			}
			itemKind = intentBullets
			items = append(items, strings.TrimSpace(intentBulletRE.FindStringSubmatch(line)[1]))
		case intentOrderedRE.MatchString(line):
			flushPara()
			flushTable()
			if itemKind != intentOrdered {
				flushItems()
			}
			itemKind = intentOrdered
			items = append(items, strings.TrimSpace(intentOrderedRE.FindStringSubmatch(line)[1]))
		case len(items) > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			items[len(items)-1] += " " + strings.TrimSpace(line)
		default:
			flushItems()
			flushTable()
			para = append(para, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">")))
		}
	}
	if inFence {
		blocks = append(blocks, intentBlock{kind: intentVerbatim, text: strings.Join(fence, "\n")})
	}
	flushAll()
	return blocks
}

var intentCodeLineRE = regexp.MustCompile(`^(?:import|export|const|let|var|await|async|function|return|if|for|while|class|def|from|\$|#!|[A-Za-z_][A-Za-z0-9_.]*\()|[;{}]$`)

// looksLikeCode reports a multi-line paragraph whose lines are source code (an
// example the author pasted without a fence). Joining its lines into prose would
// destroy it, so it is kept as a block of its own.
func looksLikeCode(lines []string) bool {
	if len(lines) < 2 {
		return false
	}
	codeLines := 0
	for _, l := range lines {
		if intentCodeLineRE.MatchString(strings.TrimSpace(l)) {
			codeLines++
		}
	}
	return codeLines*2 > len(lines)
}

// isIntentLabel reports a short single line that introduces what follows rather
// than stating anything itself: one sentence ending in a colon ("The ask:"), or
// a short heading-like line with no sentence punctuation ("The series").
func isIntentLabel(text string) bool {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "\n") || strings.Contains(text, "`") {
		return false
	}
	if strings.HasSuffix(text, ":") {
		return len(text) <= 200 && len(splitIntentSentences(text)) == 1
	}
	return len(text) <= 60 && !strings.ContainsAny(text[len(text)-1:], ".!?;,") && len(strings.Fields(text)) <= 8
}

// followsStructure reports whether the block after blocks[i] is a list or a
// verbatim block, so a trailing "...in this order:" sentence introduces it.
func followsStructure(blocks []intentBlock, i int) bool {
	if i+1 >= len(blocks) {
		return false
	}
	switch blocks[i+1].kind {
	case intentBullets, intentOrdered, intentVerbatim:
		return true
	}
	return false
}

// mergeShortSentences folds a sentence too short to be a point on its own
// ("This is ok.") into the one before it.
func mergeShortSentences(sentences []string) []string {
	var out []string
	for _, s := range sentences {
		if n := len(out); n > 0 && len(s) < 30 && len(out[n-1])+1+len(s) <= intentLineCap {
			out[n-1] += " " + s
			continue
		}
		out = append(out, s)
	}
	return out
}

func intentIsShort(blocks []intentBlock) bool {
	if len(blocks) != 1 || blocks[0].kind != intentParagraph {
		return false
	}
	return len(blocks[0].text) <= 300 && len(splitIntentSentences(blocks[0].text)) <= 2
}

var intentAbbreviations = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "approx": true, "cf": true,
	"mr": true, "mrs": true, "dr": true, "fig": true, "no": true, "st": true,
}

// splitIntentSentences splits prose at sentence ends, never inside a code span,
// parentheses or brackets, after a known abbreviation, or inside a token such
// as a file name or version (a sentence end needs the space after it).
func splitIntentSentences(text string) []string {
	var sentences []string
	runes := []rune(strings.TrimSpace(text))
	start, depth := 0, 0
	inCode := false
	for i, r := range runes {
		switch {
		case r == '`':
			inCode = !inCode
		case inCode:
		case r == '(' || r == '[':
			depth++
		case r == ')' || r == ']':
			if depth > 0 {
				depth--
			}
		case (r == '.' || r == '!' || r == '?') && depth == 0 && i+1 < len(runes) && unicode.IsSpace(runes[i+1]):
			j := i + 1
			for j < len(runes) && unicode.IsSpace(runes[j]) {
				j++
			}
			if j >= len(runes) || !(startsIntentSentence(runes[j]) || startsIdentifier(runes[j:])) || endsWithAbbreviation(runes[start:i+1]) {
				continue
			}
			sentences = append(sentences, strings.TrimSpace(string(runes[start:i+1])))
			start = j
		}
	}
	if rest := strings.TrimSpace(string(runes[start:])); rest != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

// startsIdentifier reports a lowercase word that is plainly a file or identifier
// ("project.json adds ..."), which can open a sentence.
func startsIdentifier(rest []rune) bool {
	if !unicode.IsLower(rest[0]) {
		return false
	}
	word := string(rest)
	if i := strings.IndexAny(word, " \t"); i >= 0 {
		word = word[:i]
	}
	return strings.ContainsAny(word, "_/") || (strings.Contains(word, ".") && !strings.HasSuffix(word, "."))
}

func startsIntentSentence(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsDigit(r) || r == '`' || r == '"' || r == '\'' || r == '(' || r == '*'
}

func endsWithAbbreviation(sentence []rune) bool {
	s := strings.ToLower(strings.TrimRight(string(sentence), ".!?"))
	if i := strings.LastIndexAny(s, " (\""); i >= 0 {
		s = s[i+1:]
	}
	return intentAbbreviations[s]
}

// intentSplitTiers are the only places a long statement is split. A plain comma
// or ", and " is not one of them: it is as often the middle of a compound
// subject ("the scripts X and Y, and the steps that called them, are removed")
// as a clause boundary, and a wrong split leaves a fragment that no longer
// means what it said. A statement with no safe place to split stays whole.
var intentSplitTiers = [][]string{
	{"; "},
	{", but ", ", so ", ", then "},
	{": "},
}

// splitIntentLong breaks a statement longer than intentLineCap into separate
// points, preferring a semicolon, then a contrasting or consequential clause,
// then a colon. It splits only outside code spans and brackets, and only where each
// side stays substantial, so a point is never left as a dangling fragment.
func splitIntentLong(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) <= intentLineCap {
		return []string{s}
	}
	for _, tier := range intentSplitTiers {
		at, sep := -1, ""
		best := len(s)
		for _, cand := range tier {
			for _, i := range intentDelimiterOffsets(s, cand) {
				if i < 30 || len(s)-i-len(cand) < 30 {
					continue
				}
				if d := abs(i - len(s)/2); d < best {
					best, at, sep = d, i, cand
				}
			}
		}
		if at < 0 {
			continue
		}
		left := strings.TrimRight(s[:at], " ,;")
		right := strings.TrimSpace(s[at+len(sep):])
		if strings.HasPrefix(sep, ", ") && sep != ", " {
			// Keep the conjunction that carries the meaning ("but", "so").
			right = strings.TrimSpace(strings.TrimPrefix(sep, ", ")) + " " + right
		}
		if sep == ": " {
			left += ":"
		}
		return append(splitIntentLong(left), splitIntentLong(capitalizeIntentPoint(right))...)
	}
	return []string{s}
}

// intentDelimiterOffsets returns where sep occurs in s outside code spans and
// brackets.
func intentDelimiterOffsets(s, sep string) []int {
	var offsets []int
	depth := 0
	inCode := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`':
			inCode = !inCode
		case '(', '[':
			if !inCode {
				depth++
			}
		case ')', ']':
			if !inCode && depth > 0 {
				depth--
			}
		}
		if !inCode && depth == 0 && strings.HasPrefix(s[i:], sep) {
			offsets = append(offsets, i)
		}
	}
	return offsets
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// capitalizeIntentPoint upper-cases the first letter of a fragment that began
// mid-sentence, unless it starts with something identifier-like.
func capitalizeIntentPoint(s string) string {
	runes := []rune(s)
	if len(runes) == 0 || !unicode.IsLower(runes[0]) {
		return s
	}
	word := strings.Fields(s)[0]
	if strings.ContainsAny(word, "_./`:-0123456789") || word != strings.ToLower(word) || hasInnerUpper(word) {
		return s
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func hasInnerUpper(word string) bool {
	for i, r := range word {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// intentOutput accumulates the rewritten intent and removes a statement a
// previous one already states in full.
type intentOutput struct {
	parts []string // rendered blocks, in order
	cur   []string // bullets of the list being built
	seen  []intentSeen
}

type intentSeen struct {
	key   string
	words map[string]bool
}

func (o *intentOutput) flushList() {
	if len(o.cur) == 0 {
		return
	}
	o.parts = append(o.parts, "- "+strings.Join(o.cur, "\n- "))
	o.cur = nil
}

// goal renders the first statement as the opening paragraph and returns the
// sentences that remain.
func (o *intentOutput) goal(sentences []string) []string {
	if len(sentences) == 0 {
		return nil
	}
	goal, rest := sentences[0], sentences[1:]
	if len(goal) < 80 && len(rest) > 0 && len(goal)+1+len(rest[0]) <= intentGoalMax {
		goal, rest = goal+" "+rest[0], rest[1:]
	}
	if len(goal) > intentGoalMax {
		parts := splitIntentLong(goal)
		goal = parts[0]
		rest = append(parts[1:], rest...)
	}
	o.remember(goal)
	o.parts = append(o.parts, codeFormatIntent(goal))
	return rest
}

func (o *intentOutput) bullets(points []string) {
	for _, p := range points {
		p = strings.TrimSpace(p)
		if p == "" || o.duplicate(p) {
			continue
		}
		o.cur = append(o.cur, boldLeadingLabel(p))
	}
}

var intentLeadLabelRE = regexp.MustCompile(`^([A-Z][A-Za-z0-9 /&'-]{2,44}):\s+(\S.*)$`)

// boldLeadingLabel turns a point that opens with a short "Label: ..." prefix
// ("Test step: ...") into a bold label followed by its text, so the points of a
// long brief can be scanned by topic.
func boldLeadingLabel(p string) string {
	if m := intentLeadLabelRE.FindStringSubmatch(p); m != nil && len(strings.Fields(m[1])) <= 6 {
		return "**" + m[1] + ":** " + codeFormatIntent(m[2])
	}
	return codeFormatIntent(p)
}

// lead renders a sentence that introduces the list or block after it.
func (o *intentOutput) lead(text string) {
	o.flushList()
	o.remember(text)
	o.parts = append(o.parts, codeFormatIntent(text))
}

func (o *intentOutput) label(text string) {
	o.flushList()
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "**") {
		text = "**" + text + "**"
	}
	o.parts = append(o.parts, text)
}

func (o *intentOutput) ordered(items []string) {
	o.flushList()
	var b strings.Builder
	for i, item := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strconv.Itoa(i+1) + ". " + codeFormatIntent(strings.TrimSpace(item)))
		o.remember(item)
	}
	o.parts = append(o.parts, b.String())
}

func (o *intentOutput) verbatim(text string) {
	o.flushList()
	o.parts = append(o.parts, text)
}

func (o *intentOutput) render() string {
	o.flushList()
	return strings.Join(o.parts, "\n\n")
}

var intentWordRE = regexp.MustCompile(`[\p{L}\p{N}_./:\-$]+`)

func intentWords(s string) (string, map[string]bool) {
	words := map[string]bool{}
	var order []string
	for _, w := range intentWordRE.FindAllString(strings.ToLower(s), -1) {
		w = strings.Trim(w, ".:-")
		if w == "" {
			continue
		}
		words[w] = true
		order = append(order, w)
	}
	return strings.Join(order, " "), words
}

var intentNegations = map[string]bool{"not": true, "no": true, "never": true, "without": true, "only": true, "don't": true, "dont": true, "nothing": true}

// duplicate reports whether p restates something already said. It is true only
// when one statement contains the other in full (same words, same order), or the
// two use the same words and every word that differs is plain prose: a
// difference in a number, an identifier, a path or a negation is a different
// requirement and is never merged.
func (o *intentOutput) duplicate(p string) bool {
	key, words := intentWords(p)
	if key == "" {
		return false
	}
	for _, prev := range o.seen {
		if prev.key == key || (len(key) >= 24 && strings.Contains(prev.key, key)) {
			return true
		}
		if len(words) >= 6 && sameWordsModuloProse(words, prev.words) {
			return true
		}
	}
	o.seen = append(o.seen, intentSeen{key: key, words: words})
	return false
}

func (o *intentOutput) remember(s string) {
	key, words := intentWords(s)
	if key != "" {
		o.seen = append(o.seen, intentSeen{key: key, words: words})
	}
}

func sameWordsModuloProse(a, b map[string]bool) bool {
	if len(a) < 6 || len(b) < 6 {
		return false
	}
	diff, union := 0, len(a)
	for w := range a {
		if !b[w] {
			diff++
			if !plainProseWord(w) {
				return false
			}
		}
	}
	for w := range b {
		if !a[w] {
			diff++
			union++
			if !plainProseWord(w) {
				return false
			}
		}
	}
	return float64(diff) <= 0.1*float64(union)
}

func plainProseWord(w string) bool {
	if intentNegations[w] {
		return false
	}
	for _, r := range w {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

var (
	intentProtectedRE = regexp.MustCompile("`[^`]*`|\\[[^\\]]*\\]\\([^)]*\\)|https?://[^\\s)>\\]]+|<[^>]+>")
	intentFileExts    = `ts|tsx|js|jsx|cjs|mjs|json|md|yml|yaml|sh|py|go|tf|ps1|swift|toml|lock|css|html|txt|sql|rs|java|kt|rb|xml`
	intentCodeRE      = regexp.MustCompile(`(?:^|[\s(\[“"'])(` + strings.Join([]string{
		`--?[a-z][a-z0-9]*(?:-[a-z0-9]+)*`,
		`\$[A-Za-z_][A-Za-z0-9_]*`,
		`(?:[A-Za-z0-9_.@*-]+/)+[A-Za-z0-9_.@*-]*[A-Za-z0-9_*]`,
		`[a-z0-9_][a-z0-9_-]*(?:\.[a-z0-9_-]+)*\.(?:` + intentFileExts + `)`,
		`\.(?:` + intentFileExts + `)`,
		`[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+`,
		`[a-z][a-z0-9]*(?:_[a-z0-9]+)+`,
		`[a-z]+(?:[A-Z][a-z0-9]+)+`,
		`(?:[A-Z][a-z0-9]+){2,}`,
		`[A-Za-z_][A-Za-z0-9_]*\(\)`,
		`[a-z][a-z0-9-]*(?::[a-z][a-z0-9-]*)+`,
	}, "|") + `)(?:[\s.,;:!?)\]”"']|$)`)
)

// intentNotCode lists slash and CamelCase words that read as ordinary prose.
var intentNotCode = map[string]bool{
	"and/or": true, "yes/no": true, "true/false": true, "input/output": true, "read/write": true,
	"client/server": true, "either/or": true, "on/off": true, "pass/fail": true, "w/o": true,
	"n/a": true, "his/her": true, "he/she": true, "javascript": true, "typescript": true,
	"github": true, "gitlab": true, "bitbucket": true, "powershell": true, "openapi": true,
	"devops": true, "youtube": true, "linkedin": true, "pagerduty": true, "openai": true,
}

// codeFormatIntent puts identifiers (paths, file names, flags, snake_case and
// camelCase names, environment variables, calls and colon-joined commands) in
// code formatting, leaving anything already in a code span, link or URL alone.
func codeFormatIntent(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range intentProtectedRE.FindAllStringIndex(text, -1) {
		b.WriteString(codeFormatPlain(text[last:loc[0]]))
		b.WriteString(text[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(codeFormatPlain(text[last:]))
	return b.String()
}

func codeFormatPlain(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	pos := 0
	for pos <= len(s) {
		m := intentCodeRE.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		tokStart, tokEnd := pos+m[2], pos+m[3]
		token := s[tokStart:tokEnd]
		b.WriteString(s[pos:tokStart])
		if intentNotCode[strings.ToLower(token)] || proseSlashPair(token) {
			b.WriteString(token)
		} else {
			b.WriteString("`" + token + "`")
		}
		pos = tokEnd
	}
	b.WriteString(s[pos:])
	return b.String()
}

// proseSlashPair reports "reviewed/merged" and similar word-pairs that read as
// prose rather than a path: two plain words whose endings match.
func proseSlashPair(token string) bool {
	parts := strings.Split(token, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		for _, r := range p {
			if !unicode.IsLetter(r) {
				return false
			}
		}
	}
	for _, suffix := range []string{"ed", "ing", "ly", "tion"} {
		if strings.HasSuffix(parts[0], suffix) && strings.HasSuffix(parts[1], suffix) {
			return true
		}
	}
	return false
}
