package status

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/stolostron/multicluster-global-hub/operator/api/operator/v1alpha4"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/config"
	"github.com/stolostron/multicluster-global-hub/pkg/utils"
)

type StatusReconciler struct {
	client.Client
	namespacedName types.NamespacedName
}

func NewStatusReconciler(c client.Client) *StatusReconciler {
	return &StatusReconciler{
		Client: c,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *StatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("statusController").
		For(&v1alpha4.MulticlusterGlobalHub{}, builder.WithPredicates(namespacePred)).
		Watches(&appsv1.Deployment{},
			&handler.EnqueueRequestForObject{}, builder.WithPredicates(namespacePred)).
		Watches(&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{}, builder.WithPredicates(namespacePred)).
		Complete(r)
}

var namespacePred = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return e.Object.GetNamespace() == utils.GetDefaultNamespace()
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectNew.GetNamespace() == utils.GetDefaultNamespace()
	},
	DeleteFunc: func(e event.DeleteEvent) bool {
		return e.Object.GetNamespace() == utils.GetDefaultNamespace()
	},
}

func (r *StatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mghList := &v1alpha4.MulticlusterGlobalHubList{}
	err := r.Client.List(ctx, mghList)
	if err != nil {
		klog.Error(err, "Failed to list MulticlusterGlobalHub")
		return ctrl.Result{}, err
	}
	if len(mghList.Items) == 0 {
		return ctrl.Result{}, nil
	}
	mgh := mghList.Items[0].DeepCopy()
	r.namespacedName = types.NamespacedName{
		Namespace: mgh.Namespace,
		Name:      mgh.Name,
	}
	// deleting the mgh
	if mgh.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// init components
	componentsStatus := initComponentsStatus(mgh)

	defer func() {
		err = r.updateMghStatus(ctx, componentsStatus)
		if err != nil {
			klog.Errorf("failed to update mgh status, err: %v", err)
		}
	}()

	// update manager/grafana/inventory api
	err = updateDeploymentComponents(ctx, r.Client, mgh.Namespace, componentsStatus)
	if err != nil {
		klog.Errorf("failed to update deployment components status:%v", err)
		return ctrl.Result{}, err
	}

	// update postgres
	err = updateStatefulsetComponents(ctx, r.Client, mgh.Namespace, componentsStatus)
	if err != nil {
		klog.Errorf("failed to update statefulset components status:%v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *StatusReconciler) updateMghStatus(ctx context.Context,
	componentsStatus map[string]v1alpha4.StatusCondition,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		curmgh := &v1alpha4.MulticlusterGlobalHub{}

		err := r.Client.Get(ctx, r.namespacedName, curmgh)
		if err != nil {
			return err
		}

		// update components
		updatedComponents, desiredComponentsStatus := needUpdateComponentsStatus(curmgh.Status.Components, componentsStatus)

		// update data retention condition
		updatedRetentionCond, desiredConds := updateRetentionConditions(curmgh)

		if !updatedComponents && !updatedRetentionCond {
			return nil
		}

		curmgh.Status.Components = desiredComponentsStatus
		curmgh.Status.Conditions = desiredConds

		err = r.Client.Status().Update(ctx, curmgh)
		return err
	})
}

// needUpdateComponentsStatus check if the components status need update
func needUpdateComponentsStatus(currentComponentsStatus, desiredComponentsStatus map[string]v1alpha4.StatusCondition,
) (bool, map[string]v1alpha4.StatusCondition) {
	returnedComponentsStatus := make(map[string]v1alpha4.StatusCondition)
	updated := false

	// copy the desiredComponents status
	for name, status := range currentComponentsStatus {
		returnedComponentsStatus[name] = status
	}

	for name, dcs := range desiredComponentsStatus {
		if _, ok := currentComponentsStatus[name]; !ok {
			returnedComponentsStatus[name] = dcs
			updated = true
			continue
		}
		if dcs.Type == currentComponentsStatus[name].Type &&
			dcs.Reason == currentComponentsStatus[name].Reason &&
			dcs.Status == currentComponentsStatus[name].Status &&
			dcs.Message == currentComponentsStatus[name].Message {
			returnedComponentsStatus[name] = currentComponentsStatus[name]
			continue
		}
		returnedComponentsStatus[name] = dcs
		updated = true
	}
	return updated, returnedComponentsStatus
}

func updateStatefulsetComponents(ctx context.Context, c client.Client,
	namespace string, componentsStatus map[string]v1alpha4.StatusCondition,
) error {
	statefulsetList := &appsv1.StatefulSetList{}
	err := c.List(ctx, statefulsetList, &client.ListOptions{
		Namespace: namespace,
	})
	if err != nil {
		klog.Errorf("failed to list deployments")
		return err
	}
	for _, statefulset := range statefulsetList.Items {
		if statefulset.Name != config.COMPONENTS_POSTGRES_NAME {
			continue
		}
		setComponentsAvailable(statefulset.Name, "StatefulSet",
			statefulset.Status.AvailableReplicas, *statefulset.Spec.Replicas, componentsStatus)
	}
	return nil
}

func updateDeploymentComponents(ctx context.Context, c client.Client,
	namespace string, componentsStatus map[string]v1alpha4.StatusCondition,
) error {
	deploymentList := &appsv1.DeploymentList{}
	err := c.List(ctx, deploymentList, &client.ListOptions{
		Namespace: namespace,
	})
	if err != nil {
		klog.Errorf("failed to list deployments")
		return err
	}
	for _, deploy := range deploymentList.Items {
		if deploy.Name != config.COMPONENTS_MANAGER_NAME &&
			deploy.Name != config.COMPONENTS_GRAFANA_NAME &&
			deploy.Name != config.COMPONENTS_INVENTORY_API_NAME {
			continue
		}
		setComponentsAvailable(deploy.Name, "Deployment",
			deploy.Status.AvailableReplicas, *deploy.Spec.Replicas, componentsStatus)
	}
	return nil
}

func setComponentsAvailable(name string, resourceType string,
	currentReplica, desiredReplica int32, componentsStatus map[string]v1alpha4.StatusCondition,
) {
	if currentReplica == desiredReplica {
		componentsStatus[name] = v1alpha4.StatusCondition{
			Kind:               resourceType,
			Name:               name,
			Type:               config.COMPONENTS_AVAILABLE,
			Status:             config.CONDITION_STATUS_TRUE,
			Reason:             config.MINIMUM_REPLICAS_AVAILABLE,
			Message:            fmt.Sprintf("Component %s have been deployed", name),
			LastTransitionTime: metav1.Time{Time: time.Now()},
		}
		return
	}
	componentsStatus[name] = v1alpha4.StatusCondition{
		Kind:               resourceType,
		Name:               name,
		Type:               config.COMPONENTS_AVAILABLE,
		Status:             config.CONDITION_STATUS_FALSE,
		Reason:             config.MINIMUM_REPLICAS_UNAVAILABLE,
		Message:            fmt.Sprintf("Component %s has been deployed but is not ready", name),
		LastTransitionTime: metav1.Time{Time: time.Now()},
	}
}

// dataRetention should at least be 1 month, otherwise it will deleted the current month partitions and records
func updateRetentionConditions(mgh *v1alpha4.MulticlusterGlobalHub) (bool, []metav1.Condition) {
	months, err := utils.ParseRetentionMonth(mgh.Spec.DataLayerSpec.Postgres.Retention)
	if err != nil {
		err = fmt.Errorf("failed to parse the retention month, err:%v", err)
		return config.NeedUpdateConditions(mgh.Status.Conditions, metav1.Condition{
			Type:    config.CONDITION_TYPE_DATABASE,
			Status:  config.CONDITION_STATUS_FALSE,
			Reason:  config.CONDITION_REASON_RETENTION_PARSED_FAILED,
			Message: err.Error(),
		})
	}

	if months < 1 {
		months = 1
	}
	msg := fmt.Sprintf("The data will be kept in the database for %d months.", months)
	return config.NeedUpdateConditions(mgh.Status.Conditions, metav1.Condition{
		Type:    config.CONDITION_TYPE_DATABASE,
		Status:  config.CONDITION_STATUS_TRUE,
		Reason:  config.CONDITION_REASON_RETENTION_PARSED,
		Message: msg,
	})
}

func initComponentsStatus(mgh *v1alpha4.MulticlusterGlobalHub) map[string]v1alpha4.StatusCondition {
	initComponents := map[string]v1alpha4.StatusCondition{
		config.COMPONENTS_MANAGER_NAME: {
			Kind:               "Deployment",
			Name:               config.COMPONENTS_MANAGER_NAME,
			Type:               config.COMPONENTS_AVAILABLE,
			Status:             config.CONDITION_STATUS_FALSE,
			Reason:             config.MINIMUM_REPLICAS_UNAVAILABLE,
			Message:            config.MESSAGE_WAIT_CREATED,
			LastTransitionTime: metav1.Time{Time: time.Now()},
		},
		config.COMPONENTS_GRAFANA_NAME: {
			Kind:               "Deployment",
			Name:               config.COMPONENTS_GRAFANA_NAME,
			Type:               config.COMPONENTS_AVAILABLE,
			Status:             config.CONDITION_STATUS_FALSE,
			Reason:             config.COMPONENTS_CREATING,
			Message:            config.MESSAGE_WAIT_CREATED,
			LastTransitionTime: metav1.Time{Time: time.Now()},
		},
	}
	if config.IsBYOPostgres() {
		initComponents[config.COMPONENTS_POSTGRES_NAME] = v1alpha4.StatusCondition{
			Kind:               "StatefulSet",
			Name:               config.COMPONENTS_POSTGRES_NAME,
			Type:               config.COMPONENTS_AVAILABLE,
			Status:             config.CONDITION_STATUS_TRUE,
			Reason:             "UseCustomizedStorage",
			Message:            "Use customized storage",
			LastTransitionTime: metav1.Time{Time: time.Now()},
		}
	}

	if config.WithInventory(mgh) {
		initComponents[config.COMPONENTS_INVENTORY_API_NAME] = v1alpha4.StatusCondition{
			Kind:               "Deployment",
			Name:               config.COMPONENTS_INVENTORY_API_NAME,
			Type:               config.COMPONENTS_AVAILABLE,
			Status:             config.CONDITION_STATUS_FALSE,
			Reason:             config.COMPONENTS_CREATING,
			Message:            config.MESSAGE_WAIT_CREATED,
			LastTransitionTime: metav1.Time{Time: time.Now()},
		}
	}
	return initComponents
}
