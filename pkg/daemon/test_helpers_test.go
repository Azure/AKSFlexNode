package daemon

import "github.com/Azure/AKSFlexNode/pkg/aksmachine"

func testGoalState(kubernetesVersion, settingsVersion string) aksmachine.GoalState {
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

func testMachineGoal(kubernetesVersion, settingsVersion string) aksmachine.MachineGoal {
	return aksmachine.MachineGoal{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           110,
		KubeletConfig: aksmachine.MachineKubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  intPointer(80),
		},
	}
}

func intPointer(value int) *int {
	return &value
}
