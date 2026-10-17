package psip

import (
	"strings"
	"unicode"
)

// Broadcasters often label every string "eng" whatever language it is in
// (a Spanish-language station's guide arrives tagged English), so the label
// cannot be trusted. GuessLanguage looks at the words instead.

var stopwords = map[string]map[string]bool{
	"eng": set("the and of to is with for his her their from that are who when they has have will about this as on by into while after"),
	"spa": set("el los las del una con por para que es su sus al como más pero también cuando donde sobre entre muy está ser hay ya lo se sin sus esta este"),
	"fra": set("les des du une pour dans est sur avec sont qui aux pas cette mais ses leur"),
}

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// GuessLanguage returns the ISO 639-2 code of the language text is most
// likely written in, and whether it is sure. Short texts are rarely sure.
func GuessLanguage(text string) (lang string, sure bool) {
	score := map[string]int{}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
	for _, w := range words {
		for l, sw := range stopwords {
			if sw[w] {
				score[l]++
			}
		}
	}
	if strings.ContainsAny(text, "ñ¿¡Ñ") {
		score["spa"] += 3
	}
	best, bestScore, second := "", 0, 0
	for l, s := range score {
		if s > bestScore {
			best, second, bestScore = l, bestScore, s
		} else if s > second {
			second = s
		}
	}
	if bestScore >= 2 && bestScore >= 2*second {
		return best, true
	}
	return "", false
}

// EventLanguages decides the language of each event of a channel: the guess
// from its own text when that is sure, else the channel's dominant guess,
// else the broadcast label.
func EventLanguages(evs []Event) []string {
	out := make([]string, len(evs))
	votes := map[string]int{}
	sure := make([]bool, len(evs))
	for i, e := range evs {
		if l, ok := GuessLanguage(e.Title + " " + e.Description); ok {
			out[i], sure[i] = l, true
			votes[l]++
		}
	}
	dominant, most := "", 0
	for l, n := range votes {
		if n > most {
			dominant, most = l, n
		}
	}
	for i, e := range evs {
		switch {
		case sure[i]:
		case dominant != "":
			out[i] = dominant
		default:
			out[i] = e.Language
		}
	}
	return out
}
