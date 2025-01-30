package utils

import (
	corev1 "k8s.io/api/core/v1"
)

// IsPodReady checks if a pod is ready by examining its phase and conditions
func IsPodReady(pod *corev1.Pod) bool {
	// Consider both Running and Succeeded pods as ready states
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
		return false
	}

	// Only check pod conditions for Running pods
	if pod.Status.Phase == corev1.PodRunning {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
				return false
			}
		}
	}

	return true
}
