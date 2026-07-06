package remote

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Run 阻塞式启动（CLI 兼容）
func Run(cfg *Config) error {
	svc := NewService()
	sessionID, err := svc.Start(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("Session registered: %s\n", sessionID)
	fmt.Printf("Proxying %s to server [stream-json]\n", cfg.Command)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down...")
	return svc.Stop()
}
