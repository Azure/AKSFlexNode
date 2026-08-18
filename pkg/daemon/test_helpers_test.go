package daemon

import "github.com/Azure/AKSFlexNode/pkg/aksmachine"

func testMachineGoal(kubernetesVersion, settingsVersion string) aksmachine.GoalState {
	return aksmachine.GoalState{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           intPointer(110),
		KubeletConfig: aksmachine.KubeletConfig{
			ImageGCHighThreshold: intPointer(85),
			ImageGCLowThreshold:  intPointer(80),
		},
	}
}

func intPointer(value int) *int {
	return &value
}
