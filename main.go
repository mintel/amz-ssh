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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	connect "github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2" // imports as package "cli"

	"github.com/nodefortytwo/amz-ssh/pkg/sshutils"
	"github.com/nodefortytwo/amz-ssh/pkg/update"
)

var version = "0.0.0"

func main() {
	rand.Seed(time.Now().Unix())
	setupSignalHandlers()
	app := &cli.App{
		Name:    "amz-ssh",
		Usage:   "connect to an ec2 instance via ec2 connect",
		Version: version,
		Action:  run,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "region",
				Aliases: []string{"r"},
				EnvVars: []string{"AWS_REGION"},
				Value:   "eu-west-1",
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
			&cli.StringSliceFlag{
				Name:    "destination",
				Aliases: []string{"d"},
				Usage:   "destination to ssh to via the bastion. This flag can be provided multiple times to allow for multple hops",
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
		Commands: []*cli.Command{
			{
				Name:   "update",
				Usage:  "Update the cli",
				Action: update.Handler,
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)

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
	if c.Bool("debug") {
		log.SetLevel(log.DebugLevel)
	}

	var tagName string
	var tagValue string

	if parts := strings.Split(c.String("tag"), ":"); len(parts) == 2 {
		tagName = parts[0]
		tagValue = parts[1]
	} else {
		return fmt.Errorf("%s is not a valid tag definition, use key:value", c.String("tag"))
	}

	ec2Client := ec2Client(c.String("region"))
	connectClient := connectClient(c.String("region"))

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

	for _, ep := range c.StringSlice("destination") {
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
	log.Info("Looking for bastion spot request")
	siro, err := getSpotRequestByTag(ctx, ec2Client, tagName, tagValue)
	if err != nil {
		return "", err
	}

	if len(siro.SpotInstanceRequests) > 0 {
		return aws.ToString(siro.SpotInstanceRequests[rand.Intn(len(siro.SpotInstanceRequests))].InstanceId), nil
	}

	log.Info("No spot requests found, looking for instance directly")
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

func ec2Client(region string) *ec2.Client {
	// Using the SDK's default configuration, loading additional config
	// and credentials values from the environment variables, shared
	// credentials, and shared configuration files
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}
	return ec2.NewFromConfig(cfg)
}

func connectClient(region string) *connect.Client {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}
	return connect.NewFromConfig(cfg)
}
