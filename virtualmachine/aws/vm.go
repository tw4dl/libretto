// Copyright 2015 Apcera Inc. All rights reserved.

// Package aws provides a standard way to create a virtual machine on AWS.
package aws

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws"
	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/aws/aws-sdk-go/service/ec2"
	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/virtualmachine"
)

const (
	noCredsCode  = "NoCredentialProviders"
	noRegionCode = "MissingRegion"

	instanceCount = 1

	defaultInstanceType = "t2.micro"
	defaultAMI          = "ami-5189a661" // ubuntu free tier
	defaultVolumeSize   = 8              // GB
	defaultDeviceName   = "/dev/sda1"
	defaultVolumeType   = "gp2"

	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0
	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	// RegionEnv is the env var for the AWS region.
	RegionEnv = "AWS_DEFAULT_REGION"

	// ProvisionTimeout is the maximum seconds to wait before failing to
	// provision.
	ProvisionTimeout = 90
	// SSHTimeout is the maximum time to wait before failing to GetSSH.
	SSHTimeout = 5 * time.Minute

	// StateStarted is the state AWS reports when the VM is started.
	StateStarted = "running"
	// StateHalted is the state AWS reports when the VM is halted.
	StateHalted = "stopped"
	// StateDestroyed is the state AWS reports when the VM is destroyed.
	StateDestroyed = "terminated"
	// StatePending is the state AWS reports when the VM is pending.
	StatePending = "pending"
)

// Compiler will complain if aws.VM doesn't implement VirtualMachine interface.
var _ virtualmachine.VirtualMachine = (*VM)(nil)

// limiter rate limits channel to prevent saturating AWS API limits.
var limiter = time.Tick(time.Millisecond * 500)

var (
	// ErrNoCreds is returned when no credentials are found in environment or
	// home directory.
	ErrNoCreds = errors.New("Missing AWS credentials")
	// ErrNoRegion is returned when a request was sent without a region.
	ErrNoRegion = errors.New("Missing AWS region")
	// ErrNoInstance is returned querying an instance, but none is found.
	ErrNoInstance = errors.New("Missing VM instance")
	// ErrNoInstanceID is returned when attempting to perform an operation on
	// an instance, but the ID is missing.
	ErrNoInstanceID = errors.New("Missing instance ID")
	// ErrProvisionTimeout is returned when the EC2 instance takes too long to
	// enter "running" state.
	ErrProvisionTimeout = errors.New("AWS provision timeout")
	// ErrNoIPs is returned when no IP addresses are found for an instance.
	ErrNoIPs = errors.New("Missing IPs for instance")
	// ErrNoSupportSuspend is returned when vm.Suspend() is called.
	ErrNoSupportSuspend = errors.New("Suspend action not supported by AWS")
	// ErrNoSupportResume is returned when vm.Resume() is called.
	ErrNoSupportResume = errors.New("Resume action not supported by AWS")
)

// VM represents an AWS EC2 virtual machine.
type VM struct {
	Name         string
	Region       string // required
	AMI          string
	InstanceType string
	InstanceID   string
	KeyPair      string // required

	DeviceName                   string
	VolumeSize                   int
	VolumeType                   string
	KeepRootVolumeOnDestroy      bool
	DeleteNonRootVolumeOnDestroy bool

	VPC           string
	Subnet        string
	SecurityGroup string

	SSHCreds            ssh.Credentials // required
	DeleteKeysOnDestroy bool
}

// GetName returns the name of the virtual machine
func (vm *VM) GetName() string {
	return vm.Name
}

// SetTag adds a tag to the VM and its attached volumes.
func (vm *VM) SetTag(key, value string) error {
	svc := getService(vm.Region)

	if vm.InstanceID == "" {
		return ErrNoInstanceID
	}

	volIDs, err := getInstanceVolumeIDs(svc, vm.InstanceID)
	if err != nil {
		return err
	}

	ids := make([]*string, 0, len(volIDs)+1)
	ids = append(ids, aws.String(vm.InstanceID))
	for _, v := range volIDs {
		ids = append(ids, aws.String(v))
	}

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: ids,
		Tags: []*ec2.Tag{
			{Key: aws.String(key),
				Value: aws.String(value)},
		},
	})
	if err != nil {
		return fmt.Errorf("Failed to create tag on VM: %v", err)
	}

	return nil
}

// Provision creates a virtual machine on AWS. It returns an error if
// there was a problem during creation, if there was a problem adding a tag, or
// if the VM takes too long to enter "running" state.
func (vm *VM) Provision() error {
	<-limiter
	svc := getService(vm.Region)

	resp, err := svc.RunInstances(instanceInfo(vm))
	if err != nil {
		return fmt.Errorf("Failed to create instance: %v", err)
	}

	if hasInstanceID(resp.Instances[0]) {
		vm.InstanceID = *resp.Instances[0].InstanceId
	} else {
		return ErrNoInstanceID
	}

	instID := []*string{
		aws.String(vm.InstanceID),
	}

	err = svc.WaitUntilInstanceExists(&ec2.DescribeInstancesInput{
		InstanceIds: instID,
	})
	if err != nil {
		return err
	}
	err = svc.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{
		InstanceIds: instID,
	})
	if err != nil {
		return err
	}

	if vm.DeleteNonRootVolumeOnDestroy {
		return setNonRootDeleteOnDestroy(svc, vm.InstanceID, true)
	}

	return nil
}

// GetIPs returns a slice of IP addresses assigned to the VM. The PublicIP or
// PrivateIP consts can be used to retrieve respective IP address type. It
// returns nil if there was an error obtaining the IPs.
func (vm *VM) GetIPs() []net.IP {
	svc := getService(vm.Region)
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return nil
	}

	inst, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return nil
	}

	if len(inst.Reservations) < 1 {
		return nil
	}
	if len(inst.Reservations[0].Instances) < 1 {
		return nil
	}

	ips := make([]net.IP, 2)
	if ip := inst.Reservations[0].Instances[0].PublicIpAddress; ip != nil {
		ips[PublicIP] = net.ParseIP(*ip)
	}
	if ip := inst.Reservations[0].Instances[0].PrivateIpAddress; ip != nil {
		ips[PrivateIP] = net.ParseIP(*ip)
	}

	return ips
}

// Destroy terminates the VM on AWS. It returns an error if AWS credentials are
// missing or if there is no instance ID.
func (vm *VM) Destroy() error {
	svc := getService(vm.Region)
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}
	_, err := svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return err
	}

	if !vm.DeleteKeysOnDestroy {
		return nil
	}

	return vm.DeleteKeyPair()
}

// GetSSH returns an SSH client that can be used to connect to a VM. An error
// is returned if the VM has no IPs.
func (vm *VM) GetSSH(opts ssh.Options) (ssh.Client, error) {
	ips := vm.GetIPs()
	if ips == nil || len(ips) < 1 {
		return nil, ErrNoIPs
	}

	cli := &ssh.SSHClient{
		Creds:   &vm.SSHCreds,
		IP:      ips[PublicIP],
		Options: opts,
		Port:    22,
	}

	if err := cli.WaitForSSH(SSHTimeout); err != nil {
		return nil, err
	}

	return cli, nil
}

// GetState returns the state of the VM, such as "running". An error is
// returned if the instance ID is missing, if there was a problem querying AWS,
// or if there are no instances.
func (vm *VM) GetState() (string, error) {
	svc := getService(vm.Region)

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return "", ErrNoInstanceID
	}

	stat, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return "", fmt.Errorf("Failed to get VM status: %v", err)
	}

	if n := len(stat.Reservations); n < 1 {
		return "", ErrNoInstance
	}
	if n := len(stat.Reservations[0].Instances); n < 1 {
		return "", ErrNoInstance
	}

	return *stat.Reservations[0].Instances[0].State.Name, nil
}

// Halt shuts down the VM on AWS.
func (vm *VM) Halt() error {
	svc := getService(vm.Region)

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	_, err := svc.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
		DryRun: aws.Bool(false),
		Force:  aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("Failed to stop instance: %v", err)
	}

	return nil
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	svc := getService(vm.Region)

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	_, err := svc.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
		DryRun: aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("Failed to start instance: %v", err)
	}

	return nil
}

// Suspend always returns an error because this isn't supported by AWS.
func (vm *VM) Suspend() error {
	return ErrNoSupportSuspend
}

// Resume always returns an error because this isn't supported by AWS.
func (vm *VM) Resume() error {
	return ErrNoSupportResume
}

// UseKeyPair uploads the public part of a keypair to AWS with a given name
// and sets the private part as the VM's private key. If the public key already
// exists, then the private key will still be assigned to this VM and the error
// will be nil.
func (vm *VM) UseKeyPair(kp *ssh.KeyPair, name string) error {
	if kp == nil {
		return errors.New("Key pair can't be nil.")
	}

	svc := getService(vm.Region)

	_, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(name),
		PublicKeyMaterial: kp.PublicKey,
		DryRun:            aws.Bool(false),
	})
	if awsErr, isAWS := err.(awserr.Error); isAWS {
		if awsErr.Code() != "InvalidKeyPair.Duplicate" {
			return err
		}
	} else if err != nil {
		return err
	}

	vm.SSHCreds.SSHPrivateKey = string(kp.PrivateKey)
	vm.KeyPair = name

	return nil
}

// DeleteKeyPair deletes the key pair set for this VM.
func (vm *VM) DeleteKeyPair() error {
	svc := getService(vm.Region)

	if vm.KeyPair == "" {
		return errors.New("Missing key pair name")
	}

	_, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: aws.String(vm.KeyPair),
		DryRun:  aws.Bool(false),
	})
	if err != nil {
		return err
	}

	vm.SSHCreds.SSHPrivateKey = ""
	return nil
}
