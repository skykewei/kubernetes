/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certs

import (
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	setutil "k8s.io/apimachinery/pkg/util/sets"
	certutil "k8s.io/client-go/pkg/util/cert"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/certs/pkiutil"
	"k8s.io/kubernetes/pkg/registry/core/service/ipallocator"
)

// TODO: Integration test cases
// no files exist => create all four files
// valid ca.{crt,key} exists => create apiserver.{crt,key}
// valid ca.{crt,key} and apiserver.{crt,key} exists => do nothing
// invalid ca.{crt,key} exists => error
// only one of the .crt or .key file exists => error

// CreatePKIAssets will create and write to disk all PKI assets necessary to establish the control plane.
// It generates a self-signed CA certificate and a server certificate (signed by the CA)
func CreatePKIAssets(cfg *kubeadmapi.MasterConfiguration, pkiDir string) error {
	altNames := certutil.AltNames{}

	// First, define all domains this cert should be signed for
	internalAPIServerFQDN := []string{
		"kubernetes",
		"kubernetes.default",
		"kubernetes.default.svc",
		fmt.Sprintf("kubernetes.default.svc.%s", cfg.Networking.DNSDomain),
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("couldn't get the hostname: %v", err)
	}
	altNames.DNSNames = append(cfg.API.ExternalDNSNames, hostname)
	altNames.DNSNames = append(altNames.DNSNames, internalAPIServerFQDN...)

	// then, add all IP addresses we're bound to
	for _, a := range cfg.API.AdvertiseAddresses {
		if ip := net.ParseIP(a); ip != nil {
			altNames.IPs = append(altNames.IPs, ip)
		} else {
			return fmt.Errorf("could not parse ip %q", a)
		}
	}
	// and lastly, extract the internal IP address for the API server
	_, n, err := net.ParseCIDR(cfg.Networking.ServiceSubnet)
	if err != nil {
		return fmt.Errorf("error parsing CIDR %q: %v", cfg.Networking.ServiceSubnet, err)
	}
	internalAPIServerVirtualIP, err := ipallocator.GetIndexedIP(n, 1)
	if err != nil {
		return fmt.Errorf("unable to allocate IP address for the API server from the given CIDR (%q) [%v]", &cfg.Networking.ServiceSubnet, err)
	}

	altNames.IPs = append(altNames.IPs, internalAPIServerVirtualIP)

	var caCert *x509.Certificate
	var caKey *rsa.PrivateKey
	// If at least one of them exists, we should try to load them
	// In the case that only one exists, there will most likely be an error anyway
	if pkiutil.CertOrKeyExist(pkiDir, kubeadmconstants.CACertAndKeyBaseName) {
		// Try to load ca.crt and ca.key from the PKI directory
		caCert, caKey, err = pkiutil.TryLoadCertAndKeyFromDisk(pkiDir, kubeadmconstants.CACertAndKeyBaseName)
		if err != nil || caCert == nil || caKey == nil {
			return fmt.Errorf("certificate and/or key existed but they could not be loaded properly")
		}

		// The certificate and key could be loaded, but the certificate is not a CA
		if !caCert.IsCA {
			return fmt.Errorf("certificate and key could be loaded but the certificate is not a CA")
		}

		fmt.Println("[certificates] Using the existing CA certificate and key.")
	} else {
		// The certificate and the key did NOT exist, let's generate them now
		caCert, caKey, err = pkiutil.NewCertificateAuthority()
		if err != nil {
			return fmt.Errorf("failure while generating CA certificate and key [%v]", err)
		}

		if err = pkiutil.WriteCertAndKey(pkiDir, kubeadmconstants.CACertAndKeyBaseName, caCert, caKey); err != nil {
			return fmt.Errorf("failure while saving CA certificate and key [%v]", err)
		}
		fmt.Println("[certificates] Generated CA certificate and key.")
	}

	// If at least one of them exists, we should try to load them
	// In the case that only one exists, there will most likely be an error anyway
	if pkiutil.CertOrKeyExist(pkiDir, kubeadmconstants.APIServerCertAndKeyBaseName) {
		// Try to load ca.crt and ca.key from the PKI directory
		apiCert, apiKey, err := pkiutil.TryLoadCertAndKeyFromDisk(pkiDir, kubeadmconstants.APIServerCertAndKeyBaseName)
		if err != nil || apiCert == nil || apiKey == nil {
			return fmt.Errorf("certificate and/or key existed but they could not be loaded properly")
		}

		fmt.Println("[certificates] Using the existing API Server certificate and key.")
	} else {
		// The certificate and the key did NOT exist, let's generate them now
		// TODO: Add a test case to verify that this cert has the x509.ExtKeyUsageServerAuth flag
		config := certutil.Config{
			CommonName: "kube-apiserver",
			AltNames:   altNames,
			Usages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		apiCert, apiKey, err := pkiutil.NewCertAndKey(caCert, caKey, config)
		if err != nil {
			return fmt.Errorf("failure while creating API server key and certificate [%v]", err)
		}

		if err = pkiutil.WriteCertAndKey(pkiDir, kubeadmconstants.APIServerCertAndKeyBaseName, apiCert, apiKey); err != nil {
			return fmt.Errorf("failure while saving API server certificate and key [%v]", err)
		}
		fmt.Println("[certificates] Generated API server certificate and key.")
	}

	fmt.Printf("[certificates] Valid certificates and keys now exist in %q\n", pkiDir)

	return nil
}

// Verify that the cert is valid for all IPs and DNS names it should be valid for
func checkAltNamesExist(IPs []net.IP, DNSNames []string, altNames certutil.AltNames) bool {
	dnsset := setutil.NewString(DNSNames...)

	for _, dnsNameThatShouldExist := range altNames.DNSNames {
		if !dnsset.Has(dnsNameThatShouldExist) {
			return false
		}
	}

	for _, ipThatShouldExist := range altNames.IPs {
		found := false
		for _, ip := range IPs {
			if ip.Equal(ipThatShouldExist) {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}
	return true
}
