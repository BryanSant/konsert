package digest

import (
	"encoding/json/v2"
	"regexp"
	"strconv"
	"strings"
)

// TestParseResult holds normalized counts and failing test names parsed from test runner output.
type TestParseResult struct {
	Runner string
	Passed int
	Failed int
	Names  []string
}

// ParseTestOutput inspects raw (possibly ANSI-colored) command output for known test runner
// formats (go test, go test -json, jest, vitest, pytest, cargo test, gradle, maven surefire,
// dotnet test) and returns the parsed summary and failing test names.
func ParseTestOutput(raw string) (TestParseResult, bool) {
	if strings.TrimSpace(raw) == "" {
		return TestParseResult{}, false
	}
	clean := StripANSI(raw)
	clean = strings.ReplaceAll(clean, "\r\n", "\n")

	parsers := []func(string) (TestParseResult, bool){
		parseGoTestJSON,
		parseGoTest,
		parseVitest,
		parseJest,
		parsePytest,
		parseCargoTest,
		parseMavenSurefire,
		parseGradle,
		parseDotnetTest,
	}
	for _, fn := range parsers {
		if res, ok := fn(clean); ok {
			res.Names = dedupStrings(res.Names)
			return res, true
		}
	}
	return TestParseResult{}, false
}

// --- go test -json ---

type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

func parseGoTestJSON(s string) (TestParseResult, bool) {
	var (
		res       TestParseResult
		matched   bool
		pkgPass   int
		pkgFail   int
		testPass  int
		testFail  int
		failNames []string
	)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" || ev.Package == "" {
			continue
		}
		matched = true
		switch ev.Action {
		case "pass":
			if ev.Test != "" {
				testPass++
			} else {
				pkgPass++
			}
		case "fail":
			if ev.Test != "" {
				testFail++
				failNames = append(failNames, ev.Test)
			} else {
				pkgFail++
			}
		}
	}
	if !matched {
		return TestParseResult{}, false
	}
	res.Runner = "go"
	if testPass > 0 || testFail > 0 {
		res.Passed = testPass
		res.Failed = testFail
	} else {
		res.Passed = pkgPass
		res.Failed = pkgFail
	}
	res.Names = failNames
	return res, res.Passed > 0 || res.Failed > 0 || len(res.Names) > 0
}

// --- go test (text) ---

var (
	goPassRe    = regexp.MustCompile(`(?m)^\s*--- PASS:\s+(\S+)`)
	goFailRe    = regexp.MustCompile(`(?m)^\s*--- FAIL:\s+(\S+)`)
	goPkgOkRe   = regexp.MustCompile(`(?m)^ok\s+\t\S+\t`)
	goPkgFailRe = regexp.MustCompile(`(?m)^FAIL\t(\S+)(?:\t|\s+\[)`)
)

func parseGoTest(s string) (TestParseResult, bool) {
	passes := goPassRe.FindAllStringSubmatch(s, -1)
	fails := goFailRe.FindAllStringSubmatch(s, -1)
	pkgOks := goPkgOkRe.FindAllString(s, -1)
	pkgFails := goPkgFailRe.FindAllStringSubmatch(s, -1)

	if len(passes) == 0 && len(fails) == 0 && len(pkgOks) == 0 && len(pkgFails) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "go"}
	if len(passes) > 0 || len(fails) > 0 {
		res.Passed = len(passes)
		res.Failed = len(fails)
		for _, m := range fails {
			res.Names = append(res.Names, m[1])
		}
	} else {
		res.Passed = len(pkgOks)
		res.Failed = len(pkgFails)
		for _, m := range pkgFails {
			res.Names = append(res.Names, m[1])
		}
	}
	return res, true
}

// --- vitest ---

var (
	// e.g. "Tests  2 failed | 15 passed (17)" or "Tests  15 passed (15)"
	vitestSummaryRe = regexp.MustCompile(`(?m)^\s*Tests\s+((?:\d+\s+(?:failed|passed|skipped|todo)\s*\|?\s*)+)\(\d+\)`)
	vitestFailRe    = regexp.MustCompile(`(?m)^\s*FAIL\s+(.+)$`)
	vitestCrossRe   = regexp.MustCompile(`(?m)^\s*[×✕]\s+(.+?)(?:\s+\d+\s*ms)?$`)
)

func parseVitest(s string) (TestParseResult, bool) {
	m := vitestSummaryRe.FindStringSubmatch(s)
	if m == nil {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "vitest"}
	res.Failed, res.Passed = extractCountPair(m[1], "failed", "passed")
	for _, fm := range vitestFailRe.FindAllStringSubmatch(s, -1) {
		name := strings.TrimSpace(fm[1])
		if name != "" {
			res.Names = append(res.Names, name)
		}
	}
	if len(res.Names) == 0 {
		for _, cm := range vitestCrossRe.FindAllStringSubmatch(s, -1) {
			name := strings.TrimSpace(cm[1])
			if name != "" {
				res.Names = append(res.Names, name)
			}
		}
	}
	return res, true
}

// --- jest ---

var (
	// e.g. "Tests:       2 failed, 15 passed, 17 total"
	jestSummaryRe = regexp.MustCompile(`(?m)^\s*Tests:\s+([^\n]+total)\s*$`)
	jestBulletRe  = regexp.MustCompile(`(?m)^\s*●\s+(.+)$`)
	jestCrossRe   = regexp.MustCompile(`(?m)^\s*[✕×]\s+(.+?)(?:\s+\(\d+\s*ms\))?$`)
)

func parseJest(s string) (TestParseResult, bool) {
	m := jestSummaryRe.FindStringSubmatch(s)
	if m == nil {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "jest"}
	res.Failed, res.Passed = extractCountPair(m[1], "failed", "passed")
	for _, bm := range jestBulletRe.FindAllStringSubmatch(s, -1) {
		name := strings.TrimSpace(bm[1])
		if name != "" && !strings.HasPrefix(name, "Test suite failed") {
			res.Names = append(res.Names, name)
		}
	}
	if len(res.Names) == 0 {
		for _, cm := range jestCrossRe.FindAllStringSubmatch(s, -1) {
			name := strings.TrimSpace(cm[1])
			if name != "" {
				res.Names = append(res.Names, name)
			}
		}
	}
	return res, true
}

// --- pytest ---

var (
	pytestSummaryRe = regexp.MustCompile(`(?m)^=+.*\b(\d+\s+(?:failed|passed|errors?)(?:,\s*\d+\s+(?:failed|passed|skipped|warnings?|errors?|xfailed|xpassed))*)\s+in\s+[0-9.]+s.*=+$`)
	pytestFailRe    = regexp.MustCompile(`(?m)^(?:FAILED|ERROR)\s+(\S+)`)
)

func parsePytest(s string) (TestParseResult, bool) {
	m := pytestSummaryRe.FindStringSubmatch(s)
	fails := pytestFailRe.FindAllStringSubmatch(s, -1)
	if m == nil && len(fails) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "pytest"}
	if m != nil {
		res.Failed, res.Passed = extractCountPair(m[1], "failed", "passed")
		errs, _ := extractCountPair(m[1], "error", "")
		res.Failed += errs
	}
	for _, fm := range fails {
		res.Names = append(res.Names, fm[1])
	}
	if res.Failed == 0 && len(res.Names) > 0 {
		res.Failed = len(res.Names)
	}
	return res, res.Passed > 0 || res.Failed > 0 || len(res.Names) > 0
}

// --- cargo test ---

var (
	cargoSummaryRe = regexp.MustCompile(`(?m)^test result:\s+\S+\.\s+(\d+)\s+passed;\s+(\d+)\s+failed;`)
	cargoFailRe    = regexp.MustCompile(`(?m)^test\s+(\S+)\s+\.\.\.\s+FAILED\s*$`)
)

func parseCargoTest(s string) (TestParseResult, bool) {
	summaries := cargoSummaryRe.FindAllStringSubmatch(s, -1)
	fails := cargoFailRe.FindAllStringSubmatch(s, -1)
	if len(summaries) == 0 && len(fails) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "cargo"}
	for _, m := range summaries {
		p, _ := strconv.Atoi(m[1])
		f, _ := strconv.Atoi(m[2])
		res.Passed += p
		res.Failed += f
	}
	for _, fm := range fails {
		res.Names = append(res.Names, fm[1])
	}
	if res.Failed == 0 && len(res.Names) > 0 {
		res.Failed = len(res.Names)
	}
	return res, res.Passed > 0 || res.Failed > 0 || len(res.Names) > 0
}

// --- maven surefire ---

var (
	mavenRunRe         = regexp.MustCompile(`(?m)^(?:\[(?:INFO|ERROR|WARNING)\]\s+)?Tests run:\s+(\d+),\s+Failures:\s+(\d+),\s+Errors:\s+(\d+),\s+Skipped:\s+(\d+)(.*)$`)
	mavenFailLineRe    = regexp.MustCompile(`(?m)^(?:\[ERROR\]\s+)?\s{2,}([A-Za-z0-9_$.#]+(?:\([^)]*\))?)(?::\d+)?\s+`)
	mavenFailSectionRe = regexp.MustCompile(`(?m)^(?:\[ERROR\]\s+)?(?:Failed tests:|Failures:|Errors:|Tests in error:)`)
)

func parseMavenSurefire(s string) (TestParseResult, bool) {
	matches := mavenRunRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "maven"}

	// Prefer the aggregate summary line (no " - in <class>" suffix); fall back to summing per-class lines.
	var aggMatch []string
	for _, m := range matches {
		if !strings.Contains(m[5], " - in ") && !strings.Contains(m[5], " in ") {
			aggMatch = m
		}
	}
	if aggMatch != nil {
		run, _ := strconv.Atoi(aggMatch[1])
		fail, _ := strconv.Atoi(aggMatch[2])
		errs, _ := strconv.Atoi(aggMatch[3])
		skip, _ := strconv.Atoi(aggMatch[4])
		res.Failed = fail + errs
		res.Passed = max(run-fail-errs-skip, 0)
	} else {
		for _, m := range matches {
			run, _ := strconv.Atoi(m[1])
			fail, _ := strconv.Atoi(m[2])
			errs, _ := strconv.Atoi(m[3])
			skip, _ := strconv.Atoi(m[4])
			res.Failed += fail + errs
			res.Passed += max(run-fail-errs-skip, 0)
		}
	}

	if loc := mavenFailSectionRe.FindStringIndex(s); loc != nil {
		tail := s[loc[0]:]
		for _, line := range strings.Split(tail, "\n")[1:] {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "[ERROR]"))
			if trimmed == "" || strings.HasPrefix(trimmed, "Tests run:") || strings.HasPrefix(trimmed, "[INFO]") {
				if strings.HasPrefix(trimmed, "Tests run:") {
					break
				}
				continue
			}
			if m := mavenFailLineRe.FindStringSubmatch(line); m != nil {
				res.Names = append(res.Names, m[1])
			}
		}
	}
	return res, true
}

// --- gradle ---

var (
	gradleSummaryRe = regexp.MustCompile(`(?m)(\d+)\s+tests?\s+completed,\s+(\d+)\s+failed`)
	gradleFailRe    = regexp.MustCompile(`(?m)^([A-Za-z0-9_$.]+\s+>\s+\S+(?:\([^)]*\))?)\s+FAILED\s*$`)
	gradlePassRe    = regexp.MustCompile(`(?m)^([A-Za-z0-9_$.]+\s+>\s+\S+(?:\([^)]*\))?)\s+PASSED\s*$`)
)

func parseGradle(s string) (TestParseResult, bool) {
	sum := gradleSummaryRe.FindStringSubmatch(s)
	fails := gradleFailRe.FindAllStringSubmatch(s, -1)
	passes := gradlePassRe.FindAllStringSubmatch(s, -1)
	if sum == nil && len(fails) == 0 && len(passes) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "gradle"}
	if sum != nil {
		completed, _ := strconv.Atoi(sum[1])
		failed, _ := strconv.Atoi(sum[2])
		res.Failed = failed
		res.Passed = max(completed-failed, 0)
	} else {
		res.Passed = len(passes)
		res.Failed = len(fails)
	}
	for _, fm := range fails {
		res.Names = append(res.Names, strings.TrimSpace(fm[1]))
	}
	return res, res.Passed > 0 || res.Failed > 0 || len(res.Names) > 0
}

// --- dotnet test ---

var (
	dotnetOldSummaryRe = regexp.MustCompile(`(?m)^(?:Passed!|Failed!)\s+-\s+Failed:\s*(\d+),\s+Passed:\s*(\d+),\s+Skipped:\s*\d+,\s+Total:\s*\d+`)
	dotnetNewSummaryRe = regexp.MustCompile(`(?m)^Test summary:\s+total:\s*\d+,\s+failed:\s*(\d+),\s+succeeded:\s*(\d+)`)
	dotnetFailRe       = regexp.MustCompile(`(?m)^\s*Failed\s+([A-Za-z0-9_$.<>]+(?:\([^)]*\))?)\s+\[`)
)

func parseDotnetTest(s string) (TestParseResult, bool) {
	oldMatches := dotnetOldSummaryRe.FindAllStringSubmatch(s, -1)
	newMatches := dotnetNewSummaryRe.FindAllStringSubmatch(s, -1)
	fails := dotnetFailRe.FindAllStringSubmatch(s, -1)
	if len(oldMatches) == 0 && len(newMatches) == 0 && len(fails) == 0 {
		return TestParseResult{}, false
	}
	res := TestParseResult{Runner: "dotnet"}
	for _, m := range oldMatches {
		f, _ := strconv.Atoi(m[1])
		p, _ := strconv.Atoi(m[2])
		res.Failed += f
		res.Passed += p
	}
	for _, m := range newMatches {
		f, _ := strconv.Atoi(m[1])
		p, _ := strconv.Atoi(m[2])
		res.Failed += f
		res.Passed += p
	}
	for _, fm := range fails {
		res.Names = append(res.Names, fm[1])
	}
	if res.Failed == 0 && len(res.Names) > 0 {
		res.Failed = len(res.Names)
	}
	return res, res.Passed > 0 || res.Failed > 0 || len(res.Names) > 0
}

// --- helpers ---

var countTokenRe = regexp.MustCompile(`(\d+)\s+([a-zA-Z]+)`)

func extractCountPair(s, keyA, keyB string) (int, int) {
	var a, b int
	for _, m := range countTokenRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		word := strings.ToLower(m[2])
		if keyA != "" && (word == keyA || strings.HasPrefix(word, keyA)) {
			a += n
		}
		if keyB != "" && (word == keyB || strings.HasPrefix(word, keyB)) {
			b += n
		}
	}
	return a, b
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
