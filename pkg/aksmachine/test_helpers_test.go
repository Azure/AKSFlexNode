package aksmachine

func testGoal(kubernetesVersion, settingsVersion string) GoalState {
	return GoalState{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           110,
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
}

func testMachineGoal(kubernetesVersion, settingsVersion string) MachineGoal {
	return MachineGoal{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           ptr(110),
		KubeletConfig: MachineKubeletConfig{
			ImageGCHighThreshold: ptr(85),
			ImageGCLowThreshold:  ptr(80),
		},
	}
}
