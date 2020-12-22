// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package transport

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	esv1 "github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/certificates"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/driver"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/events"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func CustomTransportCertsWatchKey(es types.NamespacedName) string {
	return esv1.ESNamer.Suffix(es.Name, "custom-transport-certs")
}

// ReconcileOrRetrieveCA either reconciles a self-signed CA generated by the operator
// or it retrieves a user defined CA certificate.
func ReconcileOrRetrieveCA(
	driver driver.Interface,
	es esv1.Elasticsearch,
	labels map[string]string,
	rotationParams certificates.RotationParams,
) (*certificates.CA, error) {
	esNSN := k8s.ExtractNamespacedName(&es)

	// Set up a dynamic watch to re-reconcile if users change or recreate the custom certificate secret. But also run this
	// to remove previously created watches if a user removes the custom certificate and goes back to operator generated certs.
	if err := certificates.ReconcileCustomCertWatch(
		driver.DynamicWatches(),
		CustomTransportCertsWatchKey(esNSN),
		esNSN,
		es.Spec.Transport.TLS.Certificate,
	); err != nil {
		return nil, err
	}

	customCASecret, err := certificates.GetSecretFromRef(driver.K8sClient(), esNSN, es.Spec.Transport.TLS.Certificate)
	if err != nil {
		// error should already contain enough context including the name of the secret
		driver.Recorder().Eventf(&es, corev1.EventTypeWarning, events.EventReasonUnexpected, err.Error())
		return nil, err
	}
	// 1. No custom certs are specified, reconcile our internal self-signed CA instead (probably the common case)
	if customCASecret == nil {
		return certificates.ReconcileCAForOwner(
			driver.K8sClient(),
			esv1.ESNamer,
			&es,
			labels,
			certificates.TransportCAType,
			rotationParams,
		)
	}

	// 2. Assuming from here on the user wants to use custom certs and has configured a secret with them.

	// Try to parse the provided secret to get to the CA and to report any validation errors to the user.
	ca, err := certificates.ParseCustomCASecret(*customCASecret)
	if err != nil {
		// Surface validation/parsing errors to the user via an event otherwise they might be hard to spot
		// validation at admission would also be an alternative but seems quite costly and secret contents might change
		// in the time between admission and reading the secret contents so we need to re-run validation here anyway.
		driver.Recorder().Eventf(&es, corev1.EventTypeWarning, events.EventReasonValidation, err.Error())
		return nil, err
	}

	// Garbage collect the self-signed CA secret which might be left over from an earlier revision on a best effort basis.
	err = driver.K8sClient().Delete(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      certificates.CAInternalSecretName(esv1.ESNamer, esNSN.Name, certificates.TransportCAType),
			Namespace: esNSN.Namespace,
		},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		log.Info("Failed to garbage collect self-signed transport CA secret, non-critical, continuing",
			"namespace", esNSN.Namespace, "name", esNSN.Name, "err", err.Error(),
		)
	}

	return ca, nil
}
