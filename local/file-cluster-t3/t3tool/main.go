// T3's file-cluster helper (EVSB-M2-17; local/file-cluster-t3):
//
//	t3tool ntp -listen :123              a fake chrony: answers NTP from this machine's clock, run in T3's containers,
//	                                     since the fork's hosts take no write without a clock source (§19.6's clocks row)
//	t3tool buckets -clusters <yml>       makes each cluster's bucket, unversioned and with no lifecycle rule, as xops
//	                                     makes production's, on T3's loopback endpoints only
//
// Nothing here reaches anything but T3's own containers: buckets refuses an endpoint off loopback or at or under port
// 50000.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: t3tool ntp -listen <address> | t3tool buckets -clusters <file>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "ntp":
		err = serveNtp(os.Args[2:])
	case "buckets":
		err = makeBuckets(os.Args[2:])
	default:
		err = fmt.Errorf("no command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "t3tool: %v\n", err)
		os.Exit(1)
	}
}

// Seconds from NTP's epoch, 1900, to Unix's.
const ntpEpochOffset = 2208988800

func ntpTimestamp(when time.Time) uint64 {
	seconds := uint64(when.Unix()) + ntpEpochOffset
	fraction := (uint64(when.Nanosecond()) << 32) / 1000000000
	return seconds<<32 | fraction
}

// Answers every NTP request from this clock, stratum 2, synchronised.
func serveNtp(args []string) error {
	flags := flag.NewFlagSet("ntp", flag.ContinueOnError)
	listen := flags.String("listen", ":123", "the UDP address to answer on")
	if err := flags.Parse(args); err != nil {
		return err
	}
	conn, err := net.ListenPacket("udp4", *listen)
	if err != nil {
		return err
	}
	request := make([]byte, 128)
	for {
		size, peer, err := conn.ReadFrom(request)
		if err != nil {
			return err
		}
		if size < 48 {
			continue
		}
		now := time.Now()
		reply := make([]byte, 48)
		reply[0] = 0<<6 | 4<<3 | 4
		reply[1] = 2
		reply[2] = request[2]
		reply[3] = 0xEC
		// root delay and dispersion, a fraction of a millisecond
		binary.BigEndian.PutUint32(reply[4:], 0x10)
		binary.BigEndian.PutUint32(reply[8:], 0x10)
		copy(reply[12:], "T3T3")
		binary.BigEndian.PutUint64(reply[16:], ntpTimestamp(now))
		copy(reply[24:32], request[40:48])
		binary.BigEndian.PutUint64(reply[32:], ntpTimestamp(now))
		binary.BigEndian.PutUint64(reply[40:], ntpTimestamp(time.Now()))
		conn.WriteTo(reply, peer)
	}
}

// Makes each cluster's bucket through its first endpoint.
func makeBuckets(args []string) error {
	flags := flag.NewFlagSet("buckets", flag.ContinueOnError)
	clustersPath := flags.String("clusters", "", "the clusters file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	text, err := os.ReadFile(*clustersPath)
	if err != nil {
		return err
	}
	configs, err := server.ParseFileClusterConfigs(text)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, config := range configs {
		endpoint := config.Endpoints[0]
		host, portText, err := net.SplitHostPort(endpoint)
		port, portErr := strconv.Atoi(portText)
		if err != nil || portErr != nil || !net.ParseIP(host).IsLoopback() || port <= 50000 {
			return fmt.Errorf("%s: %q is not a T3 endpoint on loopback above port 50000", config.ClusterId, endpoint)
		}
		client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""), Region: config.Region})
		if err != nil {
			return err
		}
		for {
			exists, err := client.BucketExists(ctx, config.Bucket)
			if err == nil && exists {
				break
			}
			if err == nil {
				if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region}); err == nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: no bucket made: %v", config.ClusterId, err)
			case <-time.After(time.Second):
			}
		}
		fmt.Printf("%s: bucket %s\n", config.ClusterId, config.Bucket)
	}
	return nil
}
