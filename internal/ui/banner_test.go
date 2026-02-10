package ui

import (
	"fmt"
	"testing"
)

func TestBannerEmbed(t *testing.T) {
	fmt.Printf("bannerText len: %d\n", len(bannerText))
	fmt.Printf("bannerText repr: %q\n", bannerText)
	if len(bannerText) == 0 {
		t.Fatal("bannerText is empty — go:embed failed")
	}
}
