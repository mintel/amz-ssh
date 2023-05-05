package sshutils

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	connect "github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	connecttypes "github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect/types"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

type EC2Endpoint struct {
	InstanceID string
	Port       int
	User       string
	PrivateKey string
	PublicKey  string
	UsePrivate bool

	Instance      *ec2types.Instance
	EC2Client     *ec2.Client
	ConnectClient *connect.Client
}

func NewEC2Endpoint(ctx context.Context, InstanceID string, ec2Client *ec2.Client, connectClient *connect.Client) (*EC2Endpoint, error) {
	endpoint := EC2Endpoint{
		InstanceID:    InstanceID,
		User:          "ec2-user",
		Port:          22,
		EC2Client:     ec2Client,
		ConnectClient: connectClient,
	}
	var err error

	if parts := strings.Split(endpoint.InstanceID, "@"); len(parts) > 1 {
		endpoint.User = parts[0]
		endpoint.InstanceID = parts[1]
	}

	if parts := strings.Split(endpoint.InstanceID, ":"); len(parts) > 1 {
		endpoint.InstanceID = parts[0]
		endpoint.Port, _ = strconv.Atoi(parts[1])
	}

	endpoint.PrivateKey, endpoint.PublicKey, err = GenerateKeys()
	if err != nil {
		return &endpoint, err
	}

	endpoint.Instance, err = getEC2Instance(ctx, endpoint.InstanceID, endpoint.EC2Client)
	if err != nil {
		return &endpoint, err
	}

	return &endpoint, nil
}

func (e *EC2Endpoint) String() string {
	err := sendPublicKey(context.TODO(), e.Instance, e.User, e.PublicKey, e.ConnectClient)
	if err != nil {
		log.Fatal(err)
	}
	if e.UsePrivate {
		return fmt.Sprintf("%s:%d", aws.ToString(e.Instance.PrivateIpAddress), e.Port)
	}

	return fmt.Sprintf("%s:%d", aws.ToString(e.Instance.PublicIpAddress), e.Port)
}

func (e *EC2Endpoint) GetSSHConfig() (*ssh.ClientConfig, error) {
	key, err := ssh.ParsePrivateKey([]byte(e.PrivateKey))
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User: e.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(key),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}, nil
}

func sendPublicKey(ctx context.Context, instance *ec2types.Instance, user, publicKey string, client *connect.Client) error {

	out, err := client.SendSSHPublicKey(ctx, &connect.SendSSHPublicKeyInput{
		AvailabilityZone: instance.Placement.AvailabilityZone,
		InstanceId:       instance.InstanceId,
		InstanceOSUser:   aws.String(user),
		SSHPublicKey:     aws.String(publicKey),
	})

	if err != nil {
		var te *connecttypes.ThrottlingException
		if errors.As(err, &te) {
			log.Debug("Got throttling exception, usually just means the key is already valid")
			return nil
		}

		return fmt.Errorf("send public key error: %w", err)
	}

	if !out.Success {
		return fmt.Errorf("request failed but no error was returned. Request ID: %s", aws.ToString(out.RequestId))
	}

	return nil
}

func getEC2Instance(ctx context.Context, id string, client *ec2.Client) (*ec2types.Instance, error) {
	instanceOutput, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})

	if err != nil {
		return nil, err
	}

	if len(instanceOutput.Reservations) == 0 || len(instanceOutput.Reservations[0].Instances) == 0 {
		return nil, errors.New("instance not found")
	}

	return &instanceOutput.Reservations[0].Instances[0], nil
}
