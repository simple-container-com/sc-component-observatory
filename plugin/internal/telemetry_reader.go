// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	awsprov "github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// TelemetryReaderInput provisions read-only telemetry access for the
// observatory service's own identity.
type TelemetryReaderInput struct {
	ComponentResourceName string
	AwsProvider           *awsprov.Provider

	// LambdaRoleName is the execution role of the deployed observatory
	// service, when the dispatched template published it. The policy
	// attaches here, so the service reads telemetry as itself with no
	// credential to store, rotate or leak.
	//
	// Empty means the template published no role; the caller then falls
	// back to creating a standalone role (see TelemetryReaderOutput).
	LambdaRoleName sdk.StringOutput
	HasLambdaRole  bool
}

// TelemetryReaderOutput reports what was provisioned.
type TelemetryReaderOutput struct {
	PolicyArn sdk.StringOutput
	// RoleName is set only when a standalone role had to be created
	// because no execution role was available to attach to.
	RoleName sdk.StringOutput
	Attached bool
}

// telemetryReadActions is the least-privilege set the dashboard needs.
//
// Derived from what the service actually calls, not from a convenience
// wildcard: a managed policy like ReadOnlyAccess would grant read on
// every service in the account, which is both more than observatory
// needs and more than an operator should have to accept to get a
// dashboard.
//
// Logs — read and search log groups:
//   - DescribeLogGroups / DescribeLogStreams resolve an inferred
//     log-group PREFIX to concrete groups (the Pulumi-generated
//     function suffix is not derivable, so the handle is a prefix).
//   - FilterLogEvents / GetLogEvents tail and search.
//   - StartQuery / GetQueryResults / StopQuery run Insights for
//     anything a filter cannot express.
//
// Metrics — read time series:
//   - GetMetricData / GetMetricStatistics for the charts.
//   - ListMetrics to discover which dimensions actually exist, rather
//     than rendering an empty chart for a metric the service never
//     emitted.
//
// Discovery — resolve a stack name to its live resources. Needed
// because the registry records what SC deployed, while these answer
// what is actually running:
//   - lambda:ListFunctions / GetFunction / ListTags
//   - ecs:ListClusters / ListServices / DescribeServices / ListTasks /
//     DescribeTasks
//
// Everything is read-only. No Put, no Delete, no Create: a dashboard
// that can change infrastructure is a liability, and nothing here
// needs to.
var telemetryReadActions = []string{
	"logs:DescribeLogGroups",
	"logs:DescribeLogStreams",
	"logs:FilterLogEvents",
	"logs:GetLogEvents",
	"logs:StartQuery",
	"logs:StopQuery",
	"logs:GetQueryResults",
	"logs:DescribeQueries",

	"cloudwatch:GetMetricData",
	"cloudwatch:GetMetricStatistics",
	"cloudwatch:ListMetrics",

	"lambda:ListFunctions",
	"lambda:GetFunction",
	"lambda:GetFunctionConfiguration",
	"lambda:ListTags",

	"ecs:ListClusters",
	"ecs:ListServices",
	"ecs:DescribeServices",
	"ecs:ListTasks",
	"ecs:DescribeTasks",
	"ecs:DescribeTaskDefinition",
}

// ProvisionTelemetryReader creates the read-only policy and attaches it
// to the observatory service's identity.
//
// This is the whole point of the Component owning its own permissions:
// an operator declares `type: sc-observatory` and gets a service that
// can read its fleet's logs and metrics, without writing an IAM policy,
// naming a role, or knowing that CloudWatch Insights needs three
// separate actions.
//
// Resource scoping is `*` with a documented reason rather than by
// omission: the dashboard's job is to discover and read telemetry for
// services it does not know about until they register, so it cannot
// enumerate their log-group ARNs in advance. Narrowing this to a
// prefix would break the moment a service is deployed under a naming
// scheme the policy did not anticipate — and the actions above are all
// read-only, so the blast radius of `*` is "can read telemetry", which
// is the feature.
func ProvisionTelemetryReader(ctx *sdk.Context, in TelemetryReaderInput) (*TelemetryReaderOutput, error) {
	if in.AwsProvider == nil {
		return nil, errors.New("ProvisionTelemetryReader: AwsProvider is required")
	}
	if in.ComponentResourceName == "" {
		return nil, errors.New("ProvisionTelemetryReader: ComponentResourceName is required")
	}

	policyName := fmt.Sprintf("sc-observatory-%s-telemetry-read", in.ComponentResourceName)
	policyDoc := telemetryReadPolicyJSON()

	policy, err := iam.NewPolicy(ctx, policyName, &iam.PolicyArgs{
		Name:        sdk.String(policyName),
		Description: sdk.String("Read-only CloudWatch logs/metrics + resource discovery for sc-component-observatory"),
		Policy:      sdk.String(policyDoc),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewPolicy (telemetry read)")
	}

	out := &TelemetryReaderOutput{PolicyArn: policy.Arn}

	if in.HasLambdaRole {
		// Attach to the service's own execution role. No credential is
		// created, so there is nothing to rotate or leak.
		if _, err := iam.NewRolePolicyAttachment(ctx, policyName+"-attach", &iam.RolePolicyAttachmentArgs{
			Role:      in.LambdaRoleName,
			PolicyArn: policy.Arn,
		}, sdk.Provider(in.AwsProvider)); err != nil {
			return nil, errors.Wrap(err, "iam.NewRolePolicyAttachment (telemetry read)")
		}
		out.Attached = true
		return out, nil
	}

	// No execution role to attach to — the dispatched template did not
	// publish one. Create a standalone role the operator can assume or
	// attach manually, rather than silently provisioning nothing: a
	// dashboard that cannot read telemetry should fail visibly at
	// deploy time, not present empty charts later.
	roleName := fmt.Sprintf("sc-observatory-%s-reader-role", in.ComponentResourceName)
	role, err := iam.NewRole(ctx, roleName, &iam.RoleArgs{
		Name: sdk.String(roleName),
		Description: sdk.String(
			"Standalone telemetry-reader role for sc-component-observatory; " +
				"created because the dispatched template published no execution role to attach to",
		),
		AssumeRolePolicy: sdk.String(lambdaAssumeRolePolicyJSON()),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRole (telemetry reader)")
	}
	if _, err := iam.NewRolePolicyAttachment(ctx, roleName+"-attach", &iam.RolePolicyAttachmentArgs{
		Role:      role.Name,
		PolicyArn: policy.Arn,
	}, sdk.Provider(in.AwsProvider)); err != nil {
		return nil, errors.Wrap(err, "iam.NewRolePolicyAttachment (standalone reader)")
	}
	out.RoleName = role.Name.ToStringOutput()
	return out, nil
}

func telemetryReadPolicyJSON() string {
	quoted := make([]string, 0, len(telemetryReadActions))
	for _, a := range telemetryReadActions {
		quoted = append(quoted, `"`+a+`"`)
	}
	return `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ObservatoryTelemetryRead",
      "Effect": "Allow",
      "Action": [` + strings.Join(quoted, ",") + `],
      "Resource": "*"
    }
  ]
}`
}

func lambdaAssumeRolePolicyJSON() string {
	return `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"Service": "lambda.amazonaws.com"},
      "Action": "sts:AssumeRole"
    }
  ]
}`
}
