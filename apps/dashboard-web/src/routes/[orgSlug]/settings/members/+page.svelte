<script lang="ts">
  export let data: { 
    members: Array<{ id: string; user: { name: string; email: string }; role: string }> 
  } = { members: [] };

  const roles = ['owner', 'admin', 'engineer', 'support', 'viewer'];
  
  function handleRoleChange(memberId: string, newRole: string) {
    // TODO: implement API call to update role
    console.log(`Update ${memberId} to ${newRole}`);
  }

  function handleRevokeAccess(memberId: string) {
    // TODO: implement API call to remove member
    console.log(`Revoke access for ${memberId}`);
  }
</script>

<div class="px-4 sm:px-6 lg:px-8">
  <div class="sm:flex sm:items-center">
    <div class="sm:flex-auto">
      <h1 class="text-base font-semibold leading-6 text-gray-900">Organization Members</h1>
      <p class="mt-2 text-sm text-gray-700">A list of all users in your organization including their name, email and role.</p>
    </div>
    <div class="mt-4 sm:ml-16 sm:mt-0 sm:flex-none">
      <button 
        type="button" 
        class="block rounded-md bg-indigo-600 px-3 py-2 text-center text-sm font-semibold text-white shadow-sm hover:bg-indigo-500 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-indigo-600"
      >
        Invite Member
      </button>
    </div>
  </div>
  <div class="mt-8 flow-root">
    <div class="-mx-4 -my-2 overflow-x-auto sm:-mx-6 lg:-mx-8">
      <div class="inline-block min-w-full py-2 align-middle sm:px-6 lg:px-8">
        <table class="min-w-full divide-y divide-gray-300">
          <thead>
            <tr>
              <th scope="col" class="py-3.5 pl-4 pr-3 text-left text-sm font-semibold text-gray-900 sm:pl-0">Name</th>
              <th scope="col" class="px-3 py-3.5 text-left text-sm font-semibold text-gray-900">Email</th>
              <th scope="col" class="px-3 py-3.5 text-left text-sm font-semibold text-gray-900">Role</th>
              <th scope="col" class="relative py-3.5 pl-3 pr-4 sm:pr-0">
                <span class="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200">
            {#each data.members as member}
              <tr>
                <td class="whitespace-nowrap py-4 pl-4 pr-3 text-sm font-medium text-gray-900 sm:pl-0">{member.user.name}</td>
                <td class="whitespace-nowrap px-3 py-4 text-sm text-gray-500">{member.user.email}</td>
                <td class="whitespace-nowrap px-3 py-4 text-sm text-gray-500">
                  <select
                    value={member.role}
                    on:change={(e) => handleRoleChange(member.id, e.currentTarget.value)}
                    class="mt-1 block w-full rounded-md border-gray-300 py-1.5 pl-3 pr-10 text-base focus:border-indigo-500 focus:outline-none focus:ring-indigo-500 sm:text-sm"
                  >
                    {#each roles as role}
                      <option value={role}>{role}</option>
                    {/each}
                  </select>
                </td>
                <td class="relative whitespace-nowrap py-4 pl-3 pr-4 text-right text-sm font-medium sm:pr-0">
                  <button 
                    type="button" 
                    on:click={() => handleRevokeAccess(member.id)} 
                    class="text-red-600 hover:text-red-900"
                  >
                    Revoke<span class="sr-only">, {member.user.name}</span>
                  </button>
                </td>
              </tr>
            {/each}
            {#if data.members.length === 0}
              <tr>
                <td colspan="4" class="py-8 text-center text-sm text-gray-500">
                  No members found.
                </td>
              </tr>
            {/if}
          </tbody>
        </table>
      </div>
    </div>
  </div>
</div>
