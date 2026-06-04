package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/gordonklaus/portaudio"
	"google.golang.org/grpc"
	pb "radio-bridge-emulator/proto"
)

func main() {
	configPath := "config.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("LoadConfig: %v", err)
	}

	if err := portaudio.Initialize(); err != nil {
		log.Fatalf("portaudio init: %v", err)
	}
	defer portaudio.Terminate()

	rec := newRecorder(cfg.Audio)
	go rec.run()

	pl := newPlayer()
	go pl.run()

	lis, err := net.Listen("tcp", cfg.Server.ListenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterTransceiverServiceServer(grpcServer, newTransceiverServer(cfg.Audio, rec, pl))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		grpcServer.GracefulStop()
	}()

	log.Printf("radio-bridge-emulator listening on %s", cfg.Server.ListenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("grpc serve: %v", err)
	}
}
