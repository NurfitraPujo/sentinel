<script lang="ts">
	// sentinel-user-report-issue §4.4a / fe.md rows O1-O10. The page is a thin shell: everything it
	// renders lives in $lib/components/oncall so it can be tested beside the component, which is the
	// pattern IssueRelations.test.ts and IssueAssigneePicker.test.ts already set.
	import OncallEditor from '$lib/components/oncall/OncallEditor.svelte';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();
</script>

<svelte:head>
	<title>On-call schedule</title>
</svelte:head>

<OncallEditor
	orgId={data.orgId}
	schedule={data.schedule}
	currentlyOnCall={data.currentlyOnCall}
	candidates={data.candidates}
	canWrite={data.canWrite}
	role={data.role}
/>
