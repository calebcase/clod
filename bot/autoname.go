package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Adjective + noun pools keyed by first letter so we can draw an
// alliterative pair (e.g. "angry-aardvark"). The pools are intentionally
// kept small and curated for memorable, friendly, pronounceable names —
// expand as needed. Letters with no good adjective/noun pair are simply
// absent; the picker skips to another letter on retry.
var nameAdjectives = map[rune][]string{
	'a': {"agile", "amber", "angry"},
	'b': {"bold", "brave", "brisk", "bubbly"},
	'c': {"calm", "clever", "cozy", "crisp"},
	'd': {"daring", "drowsy", "dizzy"},
	'e': {"eager", "elegant", "eerie"},
	'f': {"fancy", "feisty", "fierce", "friendly"},
	'g': {"gentle", "glossy", "gritty"},
	'h': {"happy", "hazy", "humble"},
	'i': {"icy", "ideal", "inky"},
	'j': {"jazzy", "jolly", "jumpy"},
	'k': {"keen", "kind", "kooky"},
	'l': {"lazy", "lively", "lovely", "lucky"},
	'm': {"merry", "mighty", "misty"},
	'n': {"nimble", "noble", "nosy"},
	'o': {"odd", "ornate", "orange"},
	'p': {"peppy", "plucky", "proud"},
	'q': {"quaint", "quick", "quirky"},
	'r': {"rapid", "rosy", "rusty"},
	's': {"shiny", "silly", "sleepy"},
	't': {"tidy", "tiny", "tough"},
	'u': {"upbeat", "upright"},
	'v': {"vain", "violet", "vivid"},
	'w': {"wacky", "wily", "wise"},
	'y': {"yearning", "yellow", "young"},
	'z': {"zany", "zesty", "zippy"},
}

var nameNouns = map[rune][]string{
	'a': {"aardvark", "albatross", "antelope"},
	'b': {"badger", "basilisk", "bison"},
	'c': {"cougar", "crane", "cricket"},
	'd': {"deer", "dolphin", "duck"},
	'e': {"eagle", "echidna", "elephant"},
	'f': {"falcon", "ferret", "fox"},
	'g': {"gecko", "goat", "goose"},
	'h': {"hawk", "hedgehog", "heron"},
	'i': {"ibex", "ibis", "iguana"},
	'j': {"jackal", "jaguar", "jay"},
	'k': {"kestrel", "kiwi", "koala"},
	'l': {"lemur", "lion", "lynx"},
	'm': {"marmot", "mink", "moose"},
	'n': {"narwhal", "newt", "nightjar"},
	'o': {"ocelot", "otter", "owl"},
	'p': {"panda", "pelican", "puma"},
	'q': {"quail", "quokka"},
	'r': {"rabbit", "raccoon", "raven"},
	's': {"seal", "sloth", "swan"},
	't': {"tiger", "toucan", "turtle"},
	'u': {"urchin", "urial"},
	'v': {"viper", "vole", "vulture"},
	'w': {"walrus", "weasel", "wolf"},
	'y': {"yak", "yabby"},
	'z': {"zebra", "zorilla"},
}

// generateTaskName builds a new `YYYY-MM-DD-<adjective>-<noun>` task
// name that doesn't collide with any existing directory under basePath.
// Picks an alliterative pair (same first letter) when possible to aid
// memorability. Falls back to cross-letter combinations only if a huge
// number of attempts collide, which in practice never happens.
//
// Date uses local time so the date portion matches what the user would
// have typed if they'd entered the task name by hand.
func generateTaskName(basePath string) (string, error) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Letters that have BOTH an adjective and a noun available —
	// used for alliterative picks.
	var alliterativeLetters []rune
	for l := 'a'; l <= 'z'; l++ {
		if len(nameAdjectives[l]) > 0 && len(nameNouns[l]) > 0 {
			alliterativeLetters = append(alliterativeLetters, l)
		}
	}

	// Separate letter pools for the fallback path.
	var adjLetters, nounLetters []rune
	for l := 'a'; l <= 'z'; l++ {
		if len(nameAdjectives[l]) > 0 {
			adjLetters = append(adjLetters, l)
		}
		if len(nameNouns[l]) > 0 {
			nounLetters = append(nounLetters, l)
		}
	}

	date := time.Now().Format("2006-01-02")

	pickAlliterative := func() (string, string) {
		letter := alliterativeLetters[r.Intn(len(alliterativeLetters))]
		adjs := nameAdjectives[letter]
		nouns := nameNouns[letter]
		return adjs[r.Intn(len(adjs))], nouns[r.Intn(len(nouns))]
	}
	pickAny := func() (string, string) {
		a := nameAdjectives[adjLetters[r.Intn(len(adjLetters))]]
		n := nameNouns[nounLetters[r.Intn(len(nounLetters))]]
		return a[r.Intn(len(a))], n[r.Intn(len(n))]
	}

	const alliterativeAttempts = 40
	const totalAttempts = 80
	for attempt := 0; attempt < totalAttempts; attempt++ {
		var adj, noun string
		if attempt < alliterativeAttempts {
			adj, noun = pickAlliterative()
		} else {
			adj, noun = pickAny()
		}
		name := fmt.Sprintf("%s-%s-%s", date, adj, noun)
		// Collision check: reject if the directory already exists.
		if _, err := os.Stat(filepath.Join(basePath, name)); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("couldn't pick an unused task name after %d attempts", totalAttempts)
}
