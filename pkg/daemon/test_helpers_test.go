package daemon

import "github.com/Azure/AKSFlexNode/pkg/aksmachine"

func testMachineGoal(kubernetesVersion, settingsVersion string) aksmachine.GoalState {
	return aksmachine.GoalState{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           110,
		KubeletConfig: aksmachine.KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
}
