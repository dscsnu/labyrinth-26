<script lang="ts">
    import { enhance, type SubmitFunction } from "$app/forms";
    import type { TeamOverview } from "#lib/types/Admin.type.ts";
    import type { PageProps } from "./$types";

    let { data, form }: PageProps = $props();
    let { user, teams, qtes, email, users } = $derived(data);
    const POWERUPS = ["damage_multiplier", "heart", "short_qr_cooldown", "instant_cast"];

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

<main>

<p><a href="/">home</a> · <a href="/leaderboard">leaderboard</a> · {user?.name} · admin</p>

{#if form?.code}
    <p style="color: red">{form.code}: {form.message}</p>
{:else if form?.done}
    <p style="color: green">{form.done}</p>
{/if}

<h1>Teams</h1>

<p>
    <input bind:value={filter} placeholder="filter by code, team or player" size="30" />
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
                · powerups held: {t.game.powerups.held.join(", ") || "none"}
                {#if t.game.powerups.active.length}· running: {t.game.powerups.active.map((a) => a.kind).join(", ")}{/if}
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
                    <!-- Never disabled: this page doesn't update live, so a cast
                         since it loaded wouldn't show; with nothing cooling, the
                         reset does nothing. -->
                    <button>reset cooldowns</button>
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

<h1>Quizzes</h1>

<p>One open at a time. Every player on /play gets it; a team's right answer gives it the quiz's powerup (unless it already holds one).</p>

{#each qtes as q (q.id)}
    <section>
        <p>
            <b>#{q.id}</b> · {q.status} · wins {q.powerup}
            {#if q.status !== "draft"}· {q.stats.answered} answered, {q.stats.correct} right, {q.stats.granted} given{/if}
        </p>
        {#if q.quiz}
            <p>
                {q.quiz.question}
                <br /><small>{q.quiz.options.map((o, i) => (i === q.quiz!.correct_option ? `✓ ${o}` : o)).join(" · ")}</small>
            </p>
        {/if}
        {#if q.status === "draft"}
            <form method="POST" action="?/startQte" use:enhance={confirmFirst(`Send quiz #${q.id} to every player now?`)}>
                <input type="hidden" name="qte_id" value={q.id} />
                <button>start (send to everyone)</button>
            </form>
        {:else if q.status === "open"}
            <form method="POST" action="?/endQte" use:enhance>
                <input type="hidden" name="qte_id" value={q.id} />
                <button>end now (reveal answer)</button>
            </form>
        {:else if q.quiz}
            <!-- A quiz runs once, so sending it again makes a copy and
                 starts that (the create action, filled in from this one). -->
            <form method="POST" action="?/createQte" use:enhance={confirmFirst(`Send quiz #${q.id} to every player again (as a new quiz)?`)}>
                <input type="hidden" name="question" value={q.quiz.question} />
                <input type="hidden" name="options" value={q.quiz.options.join("\n")} />
                <input type="hidden" name="correct" value={q.quiz.correct_option + 1} />
                <input type="hidden" name="powerup" value={q.powerup} />
                <input type="hidden" name="window_s" value={q.window_s ?? ""} />
                <input type="hidden" name="start" value="on" />
                <button>send again</button>
            </form>
        {/if}
    </section>
{:else}
    <p>No quizzes.</p>
{/each}

<h2>New quiz</h2>
<form method="POST" action="?/createQte" use:enhance>
    <p><label>question <input name="question" required size="60" /></label></p>
    <p><label>options, one per line<br /><textarea name="options" required rows="4" cols="40"></textarea></label></p>
    <p><label>right option's number <input name="correct" type="number" min="1" value="1" required /></label></p>
    <p>
        <label>wins
            <select name="powerup">
                {#each POWERUPS as k (k)}<option value={k}>{k}</option>{/each}
            </select>
        </label>
        <label>open for (seconds, default 30, at most 300) <input name="window_s" type="number" min="1" max="300" /></label>
    </p>
    <p><label><input type="checkbox" name="start" /> start now (otherwise save as a draft)</label></p>
    <button>create</button>
</form>

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
</main>

<!-- Plain, readable admin styling: Tailwind's reset strips the browser's
     defaults (heading sizes, button and input borders), so they're put back
     here for this page only. -->
<style>
    main {
        max-width: 60rem;
        margin: 0 auto;
        padding: 1rem;
        font: 15px/1.5 system-ui, sans-serif;
        color: #1f2328;
    }
    :global(body:has(main)) {
        background: #f6f7f9;
    }
    h1 {
        margin: 2rem 0 0.75rem;
        font-size: 1.5rem;
        font-weight: 700;
    }
    h2 {
        margin: 1.5rem 0 0.5rem;
        font-size: 1.125rem;
        font-weight: 600;
    }
    section h2 {
        margin: 0 0 0.25rem;
    }
    small {
        color: #656d76;
        font-weight: 400;
    }
    a {
        color: #0969da;
        text-decoration: underline;
    }
    p {
        margin: 0.5rem 0;
    }
    section {
        margin: 0.75rem 0;
        padding: 0.75rem 1rem;
        background: white;
        border: 1px solid #d0d7de;
        border-radius: 8px;
    }
    ul {
        margin: 0.5rem 0 0;
        padding-left: 1.25rem;
        list-style: disc;
    }
    li {
        margin: 0.25rem 0;
    }
    form {
        margin: 0.25rem 0.25rem 0.25rem 0;
    }
    button {
        padding: 0.25rem 0.75rem;
        background: white;
        border: 1px solid #8c959f;
        border-radius: 6px;
        cursor: pointer;
    }
    button:hover:not(:disabled) {
        background: #eaeef2;
    }
    button:disabled {
        opacity: 0.5;
        cursor: not-allowed;
    }
    input:not([type="checkbox"]),
    select,
    textarea {
        padding: 0.25rem 0.5rem;
        background: white;
        border: 1px solid #8c959f;
        border-radius: 6px;
        max-width: 100%;
    }
    input[type="checkbox"] {
        margin-right: 0.25rem;
    }
    label {
        margin-right: 0.75rem;
    }
</style>
