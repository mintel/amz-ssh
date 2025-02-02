package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	connect "github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	cli "github.com/urfave/cli/v2"
	"golang.org/x/crypto/ssh"
	"golang.org/x/exp/slog"

	"github.com/mintel/amz-ssh/pkg/sshutils"
)

var version = "0.0.0"

func main() {
	setupSignalHandlers()
	app := &cli.App{
		Name:      "amz-ssh",
		Usage:     "connect to an AWS EC2 instance via ec2-instance-connect",
		Version:   version,
		Action:    run,
		UsageText: "amz-ssh [options] destination [destination...]\n\nDestination can be an IP address or instance ID.\nMultiple destinations will be treated as addition ssh proxies in addition to the ssh bastion.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "region",
				Aliases: []string{"r"},
			},
			&cli.StringFlag{
				Name:  "tag",
				Value: "role:bastion",
			},
			&cli.StringFlag{
				Name:    "instance-id",
				Aliases: []string{"i"},
				Usage:   "instance id to ssh to or tunnel through",
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "user",
				Aliases: []string{"u"},
				Usage:   "OS user of bastion",
				Value:   "ec2-user",
			},
			&cli.StringFlag{
				Name:    "tunnel",
				Aliases: []string{"t"},
				Usage:   "Host to tunnel to",
			},
			&cli.IntFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Value:   22,
			},
			&cli.IntFlag{
				Name:    "local-port",
				Aliases: []string{"lp"},
				Usage:   "local port to map to, defaults to tunnel port",
			},
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "Print debug information",
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			os.Exit(ee.ExitStatus())
		}
		slog.Error(err.Error())
		os.Exit(1)
	}
}
func setupSignalHandlers() {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\nGoodbye!")
		os.Exit(0)
	}()
}

func run(c *cli.Context) error {
	level := slog.LevelInfo
	if c.Bool("debug") {
		level = slog.LevelDebug
	}
	h := slog.HandlerOptions{Level: level}.NewTextHandler(os.Stderr)
	slog.SetDefault(slog.New(h))

	var tagName string
	var tagValue string

	if parts := strings.Split(c.String("tag"), ":"); len(parts) == 2 {
		tagName = parts[0]
		tagValue = parts[1]
	} else {
		return fmt.Errorf("%s is not a valid tag definition, use key:value", c.String("tag"))
	}

	ec2Client, connectClient := getClients(c.Context, c.String("region"))

	instanceID := c.String("instance-id")
	if instanceID == "" {
		var err error
		instanceID, err = resolveBastionInstanceID(c.Context, ec2Client, tagName, tagValue)
		if err != nil {
			return err
		}
	}

	bastionAddr := fmt.Sprintf("%s@%s:%d", c.String("user"), instanceID, c.Int("port"))
	bastionEndpoint, err := sshutils.NewEC2Endpoint(c.Context, bastionAddr, ec2Client, connectClient)
	if err != nil {
		return err
	}

	if tunnel := sshutils.NewEndpoint(c.String("tunnel")); tunnel.Host != "" {
		p := c.Int("local-port")
		if p == 0 {
			p = tunnel.Port
		}
		return sshutils.Tunnel(p, tunnel, bastionEndpoint)
	}

	chain := []sshutils.EndpointIface{
		bastionEndpoint,
	}

	for _, ep := range c.Args().Slice() {
		destEndpoint, err := sshutils.NewEC2Endpoint(c.Context, ep, ec2Client, connectClient)
		if err != nil {
			return err
		}
		destEndpoint.UsePrivate = true
		chain = append(chain, destEndpoint)
	}

	return sshutils.Connect(chain...)
}

func getSpotRequestByTag(ctx context.Context, ec2Client *ec2.Client, tagName, tagValue string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return ec2Client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:" + tagName),
				Values: []string{tagValue},
			},
			{
				Name:   aws.String("state"),
				Values: []string{"active"},
			},
			{
				Name:   aws.String("status-code"),
				Values: []string{"fulfilled"},
			},
		},
	})
}

func getInstanceByTag(ctx context.Context, ec2Client *ec2.Client, tagName, tagValue string) (*ec2.DescribeInstancesOutput, error) {
	return ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:" + tagName),
				Values: []string{tagValue},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running"},
			},
		},
	})
}

func resolveBastionInstanceID(ctx context.Context, ec2Client *ec2.Client, tagName, tagValue string) (string, error) {
	slog.Debug("Looking for bastion spot request")
	siro, err := getSpotRequestByTag(ctx, ec2Client, tagName, tagValue)
	if err != nil {
		return "", err
	}

	if len(siro.SpotInstanceRequests) > 0 {
		return aws.ToString(siro.SpotInstanceRequests[rand.Intn(len(siro.SpotInstanceRequests))].InstanceId), nil
	}

	slog.Debug("No spot requests found, looking for instance directly")
	dio, err := getInstanceByTag(ctx, ec2Client, tagName, tagValue)
	if err != nil {
		return "", err
	}

	if len(dio.Reservations) > 0 {
		res := dio.Reservations[rand.Intn(len(dio.Reservations))]
		return aws.ToString(res.Instances[rand.Intn(len(res.Instances))].InstanceId), nil
	}

	return "", errors.New("unable to find any valid bastion instances")
}

func getClients(ctx context.Context, region string) (*ec2.Client, *connect.Client) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		slog.Error("unable to load SDK config", "err", err)
		os.Exit(1)
	}
	return ec2.NewFromConfig(cfg), connect.NewFromConfig(cfg)
}
