<script lang="ts">
    import { enhance, type SubmitFunction } from "$app/forms";
    import type { TeamOverview } from "#lib/types/Admin.type.ts";
    import type { PageProps } from "./$types";

    let { data, form }: PageProps = $props();
    let { user, teams, email, users } = $derived(data);

    let filter = $state("");
    let knockedOnly = $state(false);
    let shown = $derived(
        teams.filter(({ team, game }) => {
            if (knockedOnly && !game?.knocked_out) return false;
            const q = filter.trim().toLowerCase();
            return !q || team.id.includes(q) || team.name.toLowerCase().includes(q)
                || team.members.some((m) => m.name.toLowerCase().includes(q));
        }),
    );
    let knocked = $derived(teams.filter((t) => t.game?.knocked_out).length);

    const nameOf = (t: TeamOverview, id: string) => t.team.members.find((m) => m.id === id)?.name ?? id.slice(0, 8);

    // Asks before acting (enhance's cancel stops the request); the backend
    // refuses anything invalid anyway. On success the page's data reloads.
    const confirmFirst = (question: string): SubmitFunction => ({ cancel }) => {
        if (!confirm(question)) cancel();
    };
</script>

<p><a href="/">home</a> · <a href="/leaderboard">leaderboard</a> · {user?.name} · admin</p>

{#if form?.code}
    <p style="color: red">{form.code}: {form.message}</p>
{:else if form?.done}
    <p style="color: green">{form.done}</p>
{/if}

<h1>Teams</h1>

<p>
    <input bind:value={filter} placeholder="filter by code, team or player" />
    <label><input type="checkbox" bind:checked={knockedOnly} /> knocked out only ({knocked})</label>
    {shown.length} of {teams.length} teams
</p>

{#each shown as t (t.team.id)}
    <section>
        <h2>
            {t.team.name}
            <small>({t.team.id} · {t.team.status} · {t.team.members.length} members)</small>
        </h2>
        {#if t.game}
            <p>
                {#if t.game.knocked_out}<b style="color: red">KNOCKED OUT</b> ·{/if}
                health {t.game.health} · {t.game.bosses_defeated} bosses defeated
                {#if t.game.boss}· boss {t.game.boss.serial} at {t.game.boss.damage} damage{/if}
                · {t.game.cast_count} casts
                {#if t.game.cooldown_class}· class cooldown {t.game.cooldown_class}{/if}
                {#if t.game.qr_cooldowns.length}· QR cooling {t.game.qr_cooldowns.map((c) => c.waypoint_id).join(", ")}{/if}
            </p>
        {/if}
        <div>
            {#if t.team.status === "IN_GAME"}
                <form method="POST" action="?/revive" use:enhance={confirmFirst(`Revive ${t.team.name}? Full health, and its boss's attack clock restarts.`)} style="display: inline">
                    <input type="hidden" name="team_id" value={t.team.id} />
                    <button>revive (full health)</button>
                </form>
                <form method="POST" action="?/reset" use:enhance={confirmFirst(`Reset ${t.team.name}'s cooldowns?`)} style="display: inline">
                    <input type="hidden" name="team_id" value={t.team.id} />
                    <button disabled={!t.game?.cooldown_class && !t.game?.qr_cooldowns.length}>reset cooldowns</button>
                </form>
            {/if}
            <form method="POST" action="?/delete" use:enhance={confirmFirst(`Delete ${t.team.name} (${t.team.id}) and all its progress? This can't be undone.`)} style="display: inline">
                <input type="hidden" name="team_id" value={t.team.id} />
                <button>delete team</button>
            </form>
        </div>
        <ul>
            {#each t.team.members as m (m.id)}
                <li>
                    {m.name}{m.id === t.team.leader_id ? " (leader)" : ""}
                    {#if t.team.members.length === 1}
                        <small>— only member, can't be removed</small>
                    {:else}
                        <form method="POST" action="?/remove" style="display: inline"
                            use:enhance={confirmFirst(`Remove ${nameOf(t, m.id)} from ${t.team.name}?${m.id === t.team.leader_id ? " They lead it; the chosen member takes over." : ""}`)}>
                            <input type="hidden" name="team_id" value={t.team.id} />
                            <input type="hidden" name="user_id" value={m.id} />
                            {#if m.id === t.team.leader_id}
                                <label>
                                    new leader
                                    <select name="new_leader" required>
                                        <option value="" disabled selected>pick a member</option>
                                        {#each t.team.members.filter((o) => o.id !== m.id) as o (o.id)}
                                            <option value={o.id}>{o.name}</option>
                                        {/each}
                                    </select>
                                </label>
                            {/if}
                            <button>remove</button>
                        </form>
                    {/if}
                </li>
            {/each}
        </ul>
    </section>
{:else}
    <p>No teams.</p>
{/each}

<h1>Users and roles</h1>

<form method="GET">
    <input name="email" value={email ?? ""} placeholder="search by email" />
    <button>search</button>
</form>

{#if users}
    {#if users.length === 0}
        <p>No users match.</p>
    {:else}
        <ul>
            {#each users as u (u.id)}
                <li>
                    {u.name} &lt;{u.email}&gt; · <b>{u.role}</b>
                    {#if u.id === user?.id}
                        <small>— you (another admin must change your role)</small>
                    {:else}
                        <form method="POST" action="?/role" use:enhance style="display: inline">
                            <input type="hidden" name="user_id" value={u.id} />
                            <select name="role">
                                {#each ["PLAYER", "HELPER", "ADMIN"] as r (r)}
                                    <option value={r} selected={r === u.role}>{r}</option>
                                {/each}
                            </select>
                            <button>set role</button>
                        </form>
                    {/if}
                </li>
            {/each}
        </ul>
    {/if}
{/if}
