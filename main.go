package main

import (
	"fmt"
	"time"

	"github.com/esosaoh/crawl/internal"
)

func main() {
	fmt.Println("crawl starting...")

	config := internal.DefaultConfig()
	config.MaxDepth = 2
	config.MaxWorkers = 100
	config.RateLimit = 300 * time.Millisecond

	crawler := internal.NewCrawler(config)

	seedURLs := []string{
		"https://carleton.ca",
	}

	crawler.Start(seedURLs)

	fmt.Println("\ncrawl complete!")
}
