package psip

import "testing"

func TestGuessLanguage(t *testing.T) {
	for _, c := range []struct {
		text string
		want string
		sure bool
	}{
		{"Caso cerrado La abogada Ana María Polo resuelve toda clase problemas y conflictos, investigando cada uno de ellos para encontrar la mejor solución, viéndose envuelta en algunas ocasiones en las escandalosas batallas legales que se presentan en la corte.", "spa", true},
		{"Programa pagado Espacio dedicado a la publicidad de productos comerciales patrocinados por diferentes compañías.", "spa", true},
		{"Mister Rogers' Neighborhood Cellist Yo-Yo Ma visits & talks about music & feelings. Lady Aberlin tries to play the bass violin.", "eng", true},
		{"Le journal Les nouvelles du jour avec les invités et les reportages pour toute la famille", "fra", true},
		{"Caso cerrado", "", false},
		{"Paid Program", "", false},
		{"El Chavo", "", false},
		{"¿Quién es?", "spa", true},
	} {
		got, sure := GuessLanguage(c.text)
		if got != c.want || sure != c.sure {
			t.Errorf("%q: got %q/%v, want %q/%v", c.text, got, sure, c.want, c.sure)
		}
	}
}

func TestShortTitlesFollowTheChannel(t *testing.T) {
	evs := []Event{
		{Title: "Caso cerrado", Language: "eng"}, // too short to tell
		{Title: "Programa pagado", Description: "Espacio dedicado a la publicidad de productos comerciales para los consumidores.", Language: "eng"},
		{Title: "Noticias", Description: "El resumen de las noticias del día con los invitados y los reportajes de la semana.", Language: "eng"},
	}
	for i, l := range EventLanguages(evs) {
		if l != "spa" {
			t.Errorf("event %d: %q, want spa", i, l)
		}
	}
	// nothing to go on: the broadcast label stands
	if l := EventLanguages([]Event{{Title: "Paid Program", Language: "eng"}}); l[0] != "eng" {
		t.Errorf("label lost: %v", l)
	}
}
