//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "Pintu"

func isService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runService runs Pintu under the Windows service control manager.
func runService() error {
	return svc.Run(serviceName, &service{})
}

type service struct{}

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				log.Printf("Pintu berhenti: %v", err)
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-done; err != nil {
					log.Printf("Pintu berhenti: %v", err)
				}
				return false, 0
			}
		}
	}
}

// serviceCommand handles `pintu service install|uninstall|start|stop`.
func serviceCommand(args []string, configPath string) error {
	if len(args) == 0 {
		return errors.New("pemakaian: pintu service install|uninstall|start|stop [--config <file>]")
	}
	cmd, rest := args[0], args[1:]
	if len(rest) >= 2 && (rest[0] == "--config" || rest[0] == "-config") {
		configPath, rest = rest[1], rest[2:]
	}
	if len(rest) > 0 {
		return fmt.Errorf("argumen tidak dikenal: %v", rest)
	}

	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return errors.New("akses ditolak. Jalankan sebagai Administrator")
		}
		return fmt.Errorf("gagal terhubung ke service manager: %w", err)
	}
	defer m.Disconnect()

	switch cmd {
	case "install":
		return installService(m, configPath)
	case "uninstall":
		return withService(m, func(s *mgr.Service) error {
			stopService(s)
			if err := s.Delete(); err != nil {
				return err
			}
			fmt.Println("Service Pintu dihapus.")
			return nil
		})
	case "start":
		return withService(m, func(s *mgr.Service) error {
			if err := s.Start(); err != nil {
				return fmt.Errorf("gagal menjalankan service: %w", err)
			}
			fmt.Println("Service Pintu dijalankan.")
			return nil
		})
	case "stop":
		return withService(m, func(s *mgr.Service) error {
			if err := stopService(s); err != nil {
				return err
			}
			fmt.Println("Service Pintu dihentikan.")
			return nil
		})
	}
	return fmt.Errorf("perintah service tidak dikenal: %s", cmd)
}

func installService(m *mgr.Mgr, configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if configPath == "" {
		configPath = filepath.Join(filepath.Dir(exe), ".env")
	}
	if configPath, err = filepath.Abs(configPath); err != nil {
		return err
	}
	if !fileExists(configPath) {
		return fmt.Errorf("file config %s tidak ditemukan", configPath)
	}

	cfg := mgr.Config{
		DisplayName:      "Pintu",
		Description:      "Pintu: remote desktop manager untuk RDP dan SSH dari browser.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}
	action := "terpasang"
	s, err := m.OpenService(serviceName)
	if err == nil {
		// Already installed (e.g. an upgrade): point it at this exe and config.
		action = "diperbarui"
		var cur mgr.Config
		if cur, err = s.Config(); err == nil {
			cur.DisplayName, cur.Description = cfg.DisplayName, cfg.Description
			cur.StartType, cur.DelayedAutoStart = cfg.StartType, cfg.DelayedAutoStart
			cur.BinaryPathName = windows.EscapeArg(exe) + " --config " + windows.EscapeArg(configPath)
			err = s.UpdateConfig(cur)
		}
	} else {
		s, err = m.CreateService(serviceName, exe, cfg, "--config", configPath)
	}
	if err != nil {
		return fmt.Errorf("gagal memasang service: %w", err)
	}
	defer s.Close()

	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 24*60*60); err != nil {
		return fmt.Errorf("gagal mengatur recovery service: %w", err)
	}
	fmt.Printf("Service Pintu %s (config: %s).\n", action, configPath)
	return nil
}

func withService(m *mgr.Mgr, fn func(*mgr.Service) error) error {
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("service Pintu belum terpasang")
	}
	defer s.Close()
	return fn(s)
}

// stopService stops the service and waits up to 15 seconds. Already stopped is not an error.
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("gagal menghentikan service: %w", err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("service Pintu tidak berhenti dalam 15 detik")
}
