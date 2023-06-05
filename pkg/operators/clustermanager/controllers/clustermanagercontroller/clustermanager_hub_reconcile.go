/*
 * Copyright 2022 Contributors to the Open Cluster Management project
 */

package clustermanagercontroller

import (
	"context"
	"fmt"

	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	apiregistrationclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	operatorapiv1 "open-cluster-management.io/api/operator/v1"
	"open-cluster-management.io/registration-operator/manifests"
	"open-cluster-management.io/registration-operator/pkg/helpers"
)

var (
	namespaceResource = "cluster-manager/cluster-manager-namespace.yaml"

	// The hubRbacResourceFiles should be deployed in the hub cluster.
	hubRbacResourceFiles = []string{
		// registration
		"cluster-manager/hub/cluster-manager-registration-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-registration-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-registration-serviceaccount.yaml",
		// registration-webhook
		"cluster-manager/hub/cluster-manager-registration-webhook-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-registration-webhook-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-registration-webhook-serviceaccount.yaml",
		// work-webhook
		"cluster-manager/hub/cluster-manager-work-webhook-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-work-webhook-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-work-webhook-serviceaccount.yaml",
		// placement
		"cluster-manager/hub/cluster-manager-placement-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-placement-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-placement-serviceaccount.yaml",
	}

	mwReplicaSetResourceFiles = []string{
		// manifestworkreplicaset
		"cluster-manager/hub/cluster-manager-manifestworkreplicaset-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-manifestworkreplicaset-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-manifestworkreplicaset-serviceaccount.yaml",
	}

	hubAddOnManagerRbacResourceFiles = []string{
		// addon-manager
		"cluster-manager/hub/cluster-manager-addon-manager-clusterrole.yaml",
		"cluster-manager/hub/cluster-manager-addon-manager-clusterrolebinding.yaml",
		"cluster-manager/hub/cluster-manager-addon-manager-serviceaccount.yaml",
	}

	// The hubHostedWebhookServiceFiles should only be deployed on the hub cluster when the deploy mode is hosted.
	hubDefaultWebhookServiceFiles = []string{
		"cluster-manager/hub/cluster-manager-registration-webhook-service.yaml",
		"cluster-manager/hub/cluster-manager-work-webhook-service.yaml",
	}
	hubHostedWebhookServiceFiles = []string{
		"cluster-manager/hub/cluster-manager-registration-webhook-service-hosted.yaml",
		"cluster-manager/hub/cluster-manager-work-webhook-service-hosted.yaml",
	}

	// hubHostedWebhookEndpointFiles only apply when the deploy mode is hosted and address is IPFormat.
	hubHostedWebhookEndpointRegistration = "cluster-manager/hub/cluster-manager-registration-webhook-endpoint-hosted.yaml"
	hubHostedWebhookEndpointWork         = "cluster-manager/hub/cluster-manager-work-webhook-endpoint-hosted.yaml"

	hubWebhookAPIServiceNames = []string{
		"v1.admission.cluster.open-cluster-management.io",
		"v1.admission.work.open-cluster-management.io",
	}
)

type hubReoncile struct {
	hubKubeClient            kubernetes.Interface
	hubAPIRegistrationClient apiregistrationclient.APIServicesGetter

	cache    resourceapply.ResourceCache
	recorder events.Recorder
}

func (c *hubReoncile) reconcile(ctx context.Context, cm *operatorapiv1.ClusterManager, config manifests.HubConfig) (*operatorapiv1.ClusterManager, reconcileState, error) {
	// If AddOnManager is not enabled, remove related resources
	if !config.AddOnManagerEnabled {
		_, _, err := cleanResources(ctx, c.hubKubeClient, cm, config, hubAddOnManagerRbacResourceFiles...)
		if err != nil {
			return cm, reconcileStop, err
		}
	}

	// Remove ManifestWokReplicaSet deployment if feature not enabled
	if !config.MWReplicaSetEnabled {
		_, _, err := cleanResources(ctx, c.hubKubeClient, cm, config, mwReplicaSetResourceFiles...)
		if err != nil {
			return cm, reconcileStop, err
		}
	}

	hubResources := getHubResources(cm.Spec.DeployOption.Mode, config)
	var appliedErrs []error

	resourceResults := helpers.ApplyDirectly(
		ctx,
		c.hubKubeClient,
		nil,
		c.recorder,
		c.cache,
		func(name string) ([]byte, error) {
			template, err := manifests.ClusterManagerManifestFiles.ReadFile(name)
			if err != nil {
				return nil, err
			}
			objData := assets.MustCreateAssetFromTemplate(name, template, config).Data
			helpers.SetRelatedResourcesStatusesWithObj(&cm.Status.RelatedResources, objData)
			return objData, nil
		},
		hubResources...,
	)

	//TODO: Handle Upgrade case, should delete later
	err := c.cleanApiservice(ctx)
	if err != nil {
		appliedErrs = append(appliedErrs, err)
	}

	for _, result := range resourceResults {
		if result.Error != nil {
			appliedErrs = append(appliedErrs, fmt.Errorf("%q (%T): %v", result.File, result.Type, result.Error))
		}
	}

	if len(appliedErrs) > 0 {
		meta.SetStatusCondition(&cm.Status.Conditions, metav1.Condition{
			Type:    clusterManagerApplied,
			Status:  metav1.ConditionFalse,
			Reason:  "HubResourceApplyFailed",
			Message: fmt.Sprintf("Failed to apply hub resources: %v", utilerrors.NewAggregate(appliedErrs)),
		})
		return cm, reconcileStop, utilerrors.NewAggregate(appliedErrs)
	}

	return cm, reconcileContinue, nil
}

func (c *hubReoncile) cleanApiservice(ctx context.Context) error {
	var err error
	if c.hubAPIRegistrationClient == nil {
		err = fmt.Errorf("apiRegistrationClient is nil")
		return err
	}
	for _, apiServiceName := range hubWebhookAPIServiceNames {
		err = c.hubAPIRegistrationClient.APIServices().Delete(ctx, apiServiceName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
func (c *hubReoncile) clean(ctx context.Context, cm *operatorapiv1.ClusterManager, config manifests.HubConfig) (*operatorapiv1.ClusterManager, reconcileState, error) {
	hubResources := getHubResources(cm.Spec.DeployOption.Mode, config)
	return cleanResources(ctx, c.hubKubeClient, cm, config, hubResources...)
}

func getHubResources(mode operatorapiv1.InstallMode, config manifests.HubConfig) []string {
	hubResources := []string{namespaceResource}
	hubResources = append(hubResources, hubRbacResourceFiles...)
	if config.AddOnManagerEnabled {
		hubResources = append(hubResources, hubAddOnManagerRbacResourceFiles...)
	}

	if config.MWReplicaSetEnabled {
		hubResources = append(hubResources, mwReplicaSetResourceFiles...)
	}
	// the hubHostedWebhookServiceFiles are only used in hosted mode
	if mode == operatorapiv1.InstallModeHosted {
		hubResources = append(hubResources, hubHostedWebhookServiceFiles...)
		if config.RegistrationWebhook.IsIPFormat {
			hubResources = append(hubResources, hubHostedWebhookEndpointRegistration)
		}
		if config.WorkWebhook.IsIPFormat {
			hubResources = append(hubResources, hubHostedWebhookEndpointWork)
		}
	} else {
		hubResources = append(hubResources, hubDefaultWebhookServiceFiles...)
	}

	return hubResources
}
