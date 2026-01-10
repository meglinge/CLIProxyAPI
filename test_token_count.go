// +build ignore

package main

import (
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
)

func main() {
	data, err := os.ReadFile("未命名.txt")
	if err != nil {
		fmt.Println("Error reading file:", err)
		return
	}

	e := executor.NewTokenEstimator()

	systemTokens := e.EstimateSystemTokens(data)
	messagesTokens := e.EstimateMessagesTokens(data)
	toolsTokens := e.EstimateToolsTokens(data)
	totalTokens := e.EstimateTotalTokens(data)

	fmt.Printf("System tokens:   %d\n", systemTokens)
	fmt.Printf("Messages tokens: %d\n", messagesTokens)
	fmt.Printf("Tools tokens:    %d\n", toolsTokens)
	fmt.Printf("Total tokens:    %d\n", totalTokens)
	fmt.Printf("\nFile size: %d bytes\n", len(data))
}
