package text

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
)

// fussyNER finds NAME only when it is isolated on its own line, like the real
// NER models that miss names inside JSON-escaped / CSV / comment context.
type fussyNER struct{ name string }

func (f fussyNER) Analyze(_ context.Context, text string, _ []string, _ string) ([]presidio.Detection, error) {
	var out []presidio.Detection
	off := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.TrimSpace(line) == f.name {
			s := off + strings.Index(line, f.name)
			out = append(out, presidio.Detection{EntityType: "PERSON", Start: s, End: s + len(f.name), Text: f.name, Score: 0.85})
		}
		off += len(line)
	}
	return out, nil
}

func fussyHandler(t *testing.T, a Analyzer, allow map[string]struct{}) *Handler {
	mr := miniredis.RunT(t)
	store := mapping.NewRedisStoreFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	return NewHandler(HandlerConfig{
		Analyzer:    a,
		Store:       store,
		Strategizer: NewStrategizer(StrategizerConfig{Pools: NewPools(nil), Default: string(StrategyToken)}),
		Entities:    []string{"PERSON", "ORGANIZATION"},
		Languages:   []string{"en", "ru"},
		AllowList:   allow,
	})
}

func TestFirstSighting_ToolOutputShapes(t *testing.T) {
	for _, name := range []string{"Святослав Иванов", "Sviatoslav Ivanov", "Nikolai Petrov"} {
		for _, tmpl := range []string{
			`{"chunk_id":"0e4afc","exit_code":0,"output":"1:# Maintainer: NAME\n"}`,
			"id,name,email\n1,NAME,x@example.org\n",
			"src/app.py:12:    // TODO(NAME): fix\n",
			"{'author': 'NAME', 'note': \"he said \\\"hi\\\"\"}",
			"Author: NAME <a@example.com>\nDate: Mon",
		} {
			txt := strings.ReplaceAll(tmpl, "NAME", name)
			h := fussyHandler(t, fussyNER{name}, nil)
			res, err := h.Pseudonymize(context.Background(), txt, "s")
			require.NoError(t, err)
			require.NotContains(t, res.Text, name, txt)
			require.Equal(t, 1, res.NewMappingsCreated)
		}
	}
}

func TestExplodeViews_SameLength(t *testing.T) {
	in := `{"a":"x\n\"y\" Иванов Don't"}`
	vs := explodeViews(in)
	require.Len(t, vs, 2)
	for _, v := range vs {
		require.Equal(t, len(in), len(v))
		require.False(t, strings.ContainsAny(v, `{}":\`))
		require.Contains(t, v, "Don't")
	}
	require.Nil(t, explodeViews("plain words"))
}

func TestFilterNonPII(t *testing.T) {
	cases := []struct {
		text, span string
		typ        string
		drop       bool
	}{
		{"See CLAUDE.md for details", "CLAUDE", "PERSON", true},
		{"See CLAUDE.md for details", "CLAUDE.md", "ORGANIZATION", true},
		{"edit src/ivanov/app.py now", "ivanov", "PERSON", true},
		{"open /home/Sviatoslav Ivanov/a.txt", "Sviatoslav Ivanov", "PERSON", false},
		{"use snake_case_Name here", "Name", "PERSON", true},
		{"hosted on GitHub today", "GitHub", "ORGANIZATION", true},
		{"run the CLI tool", "CLI", "ORGANIZATION", true},
		{"visit https://example.com/Petrov", "Petrov", "PERSON", true},
		{"model GPT-5 is", "GPT", "ORGANIZATION", true},
		{"ask Nikolai Petrov today", "Nikolai Petrov", "PERSON", false},
		{"ask Святослав Иванов today", "Святослав Иванов", "PERSON", false},
		{"ask McDonald today", "McDonald", "PERSON", false},
		{"client Northwind Labs signed", "Northwind Labs", "ORGANIZATION", false},
		{"Author: Nikolai Petrov <nikolai.petrov@example.com>", "Nikolai Petrov", "PERSON", false},
	}
	for _, c := range cases {
		i := strings.Index(c.text, c.span)
		d := presidio.Detection{EntityType: c.typ, Start: i, End: i + len(c.span), Text: c.span}
		got := filterNonPII(c.text, []presidio.Detection{d})
		require.Equal(t, c.drop, len(got) == 0, "%q in %q", c.span, c.text)
	}
}

func TestAllowListed(t *testing.T) {
	allow := map[string]struct{}{"claude": {}, "claude code": {}, "code": {}, "vs code": {}}
	require.True(t, allowListed(allow, "Claude"))
	require.True(t, allowListed(allow, "CLAUDE"))
	require.True(t, allowListed(allow, "Claude Code"))
	require.True(t, allowListed(allow, "VS Code"))
	require.False(t, allowListed(allow, "Claude Monet"))
	require.False(t, allowListed(allow, "Nikolai"))
}

func TestHandler_AllowListSurvivesNER(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Start: 4, End: 10, Text: "Claude", Score: 0.9},
	}}
	h, _ := newAllowListHandler(t, stub, map[string]struct{}{"claude": {}})
	h.entities = []string{"PERSON", "ORGANIZATION"}
	res, err := h.Pseudonymize(context.Background(), "You Claude are", "s")
	require.NoError(t, err)
	require.Equal(t, "You Claude are", res.Text)
}

func TestSplitForeignRuns(t *testing.T) {
	txt := "Святослав Иванов and Коля"
	d := presidio.Detection{EntityType: "PERSON", Start: 5, End: 5 + len(txt), Text: txt, Score: 0.85}
	got := splitForeignRuns("ru", d)
	require.Len(t, got, 2)
	require.Equal(t, "Святослав Иванов", got[0].Text)
	require.Equal(t, 5, got[0].Start)
	require.Equal(t, "Коля", got[1].Text)
	require.Equal(t, 5+len("Святослав Иванов and "), got[1].Start)
}

func TestSplitAtMask(t *testing.T) {
	txt := "x Maintainer: Sviatoslav Ivanov\\n"
	mask := delimMask(txt)
	i := strings.Index(txt, "Maintainer")
	d := presidio.Detection{EntityType: "PERSON", Start: i, End: len(txt) - 2, Text: "Maintainer  Sviatoslav Ivanov"}
	got := splitAtMask(txt, mask, []presidio.Detection{d})
	require.Len(t, got, 2)
	require.Equal(t, "Maintainer", got[0].Text)
	require.Equal(t, "Sviatoslav Ivanov", got[1].Text)
}

func TestFilterNonPII_LowercaseAndBareDigits(t *testing.T) {
	text := "i ae https tmpdir contact duty) 036071417 +7 916 123-45-67 (495) 123-45-67 Margaret Holloway Коля"
	mk := func(sub, typ string) presidio.Detection {
		i := strings.Index(text, sub)
		return presidio.Detection{EntityType: typ, Start: i, End: i + len(sub), Score: 0.9, Text: sub}
	}
	in := []presidio.Detection{
		mk("i", "PERSON"), mk("ae", "ORGANIZATION"), mk("https", "ORGANIZATION"), mk("tmpdir", "PERSON"),
		mk("contact", "ORGANIZATION"), mk("duty)", "ORGANIZATION"), mk("036071417", "PHONE_NUMBER"),
		mk("+7 916 123-45-67", "PHONE_NUMBER"), mk("(495) 123-45-67", "PHONE_NUMBER"),
		mk("Margaret Holloway", "PERSON"), mk("Коля", "PERSON"),
	}
	got := map[string]bool{}
	for _, d := range filterNonPII(text, in) {
		got[d.Text] = true
	}
	for _, keep := range []string{"+7 916 123-45-67", "(495) 123-45-67", "Margaret Holloway", "Коля"} {
		if !got[keep] {
			t.Errorf("expected %q kept", keep)
		}
	}
	for _, drop := range []string{"i", "ae", "https", "tmpdir", "contact", "duty)", "036071417"} {
		if got[drop] {
			t.Errorf("expected %q dropped", drop)
		}
	}
}

func TestFilterNonPII_TrimsEdgePunct(t *testing.T) {
	text := "-Margaret Holloway and `Sviatoslav Ivanov`"
	mk := func(sub string) presidio.Detection {
		i := strings.Index(text, sub)
		return presidio.Detection{EntityType: "PERSON", Start: i, End: i + len(sub), Score: 0.9, Text: sub}
	}
	got := filterNonPII(text, []presidio.Detection{mk("-Margaret Holloway"), mk("`Sviatoslav Ivanov`")})
	if len(got) != 2 || got[0].Text != "Margaret Holloway" || got[1].Text != "Sviatoslav Ivanov" {
		t.Fatalf("unexpected: %+v", got)
	}
}

type commonNER struct{ spans []string }

func (c commonNER) Analyze(_ context.Context, text string, _ []string, _ string) ([]presidio.Detection, error) {
	var out []presidio.Detection
	for _, sp := range c.spans {
		for off := 0; ; {
			i := strings.Index(text[off:], sp)
			if i < 0 {
				break
			}
			s := off + i
			typ := "PERSON"
			out = append(out, presidio.Detection{EntityType: typ, Start: s, End: s + len(sp), Text: sp, Score: 0.85,
				RecognitionMetadata: &presidio.RecognitionMetadata{RecognizerName: "SpacyRecognizer"}})
			off = s + len(sp)
		}
	}
	return out, nil
}

func TestCommonWords_DropsNERSingleWordsKeepsNames(t *testing.T) {
	common := map[string]struct{}{"client": {}, "contact": {}, "billing": {}, "project": {}, "team": {}, "codebase": {}, "baker": {}}
	for _, w := range []string{"Client", "Contact", "Billing", "Project", "Team", "Codebase"} {
		h := fussyHandler(t, commonNER{[]string{w}}, nil)
		h.commonWords = common
		res, err := h.Pseudonymize(context.Background(), "Who is the "+w+" here? "+w+" again.", "s")
		require.NoError(t, err)
		require.Contains(t, res.Text, w, w)
		require.Equal(t, 0, res.NewMappingsCreated, w)
	}
	for _, n := range []string{"Николай Петров", "Коля", "Святослав Иванов", "Sviatoslav Ivanov", "Ярослав Кузнецов",
		"Margaret Holloway", "Nikolai Petrov", "Petrov", "Holloway"} {
		h := fussyHandler(t, commonNER{[]string{n}}, nil)
		h.commonWords = common
		res, err := h.Pseudonymize(context.Background(), "Ask "+n+" today", "s")
		require.NoError(t, err)
		require.NotContains(t, res.Text, n, n)
		require.Equal(t, 1, res.NewMappingsCreated, n)
	}
}

func TestCommonWords_PatternRecognizerAndMultiWordUnaffected(t *testing.T) {
	common := map[string]struct{}{"client": {}, "baker": {}}
	// no recognizer metadata (pattern / unknown source) -> kept
	got := filterCommonWords(common, []presidio.Detection{{EntityType: "PERSON", Text: "Client"}})
	require.Len(t, got, 1)
	ner := &presidio.RecognitionMetadata{RecognizerName: "SpacyRecognizer"}
	// multi-word NER span with a common first word -> kept
	got = filterCommonWords(common, []presidio.Detection{{EntityType: "PERSON", Text: "Client Baker", RecognitionMetadata: ner}})
	require.Len(t, got, 1)
	// EMAIL never touched
	got = filterCommonWords(common, []presidio.Detection{{EntityType: "EMAIL_ADDRESS", Text: "client", RecognitionMetadata: ner}})
	require.Len(t, got, 1)
	// nil set: no-op
	require.Len(t, filterCommonWords(nil, []presidio.Detection{{EntityType: "PERSON", Text: "Client", RecognitionMetadata: ner}}), 1)
	// NER single word on the list -> dropped (also with edge punctuation)
	require.Empty(t, filterCommonWords(common, []presidio.Detection{{EntityType: "ORGANIZATION", Text: "Client:", RecognitionMetadata: ner}}))
}
