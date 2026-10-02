<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap-reverse items-start justify-between gap-3">
          <AccountTableFilters
            v-model:searchQuery="params.search"
            :filters="params"
            :groups="groups"
            @update:filters="(newFilters) => Object.assign(params, newFilters)"
            @change="debouncedReload"
            @update:searchQuery="debouncedReload"
          />
          <AccountTableActions
            :loading="loading"
            @refresh="handleManualRefresh"
            @create="showCreate = true"
          >
            <template #after>
              <!-- Auto Refresh Dropdown -->
              <div class="relative" ref="autoRefreshDropdownRef">
                <button
                  @click="
                    showAutoRefreshDropdown = !showAutoRefreshDropdown;
                    showAccountToolsDropdown = false
                  "
                  class="btn btn-secondary px-2 md:px-3"
                  :title="t('admin.accounts.autoRefresh')"
                >
                  <Icon name="refresh" size="sm" :class="[autoRefreshEnabled ? 'animate-spin' : '']" />
                  <span class="hidden md:inline">
                    {{
                      autoRefreshEnabled
                        ? t('admin.accounts.autoRefreshCountdown', { seconds: autoRefreshCountdown })
                        : t('admin.accounts.autoRefresh')
                    }}
                  </span>
                </button>
                <div
                  v-if="showAutoRefreshDropdown"
                  class="absolute right-0 z-50 mt-2 w-56 origin-top-right rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-700 dark:bg-dark-800"
                >
                  <div class="p-2">
                    <button
                      @click="setAutoRefreshEnabled(!autoRefreshEnabled)"
                      class="flex w-full items-center justify-between rounded-md px-3 py-2 text-sm text-gray-700 hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-dark-700"
                    >
                      <span>{{ t('admin.accounts.enableAutoRefresh') }}</span>
                      <Icon v-if="autoRefreshEnabled" name="check" size="sm" class="text-primary-500" />
                    </button>
                    <div class="my-1 border-t border-gray-100 dark:border-dark-700"></div>
                    <button
                      v-for="sec in autoRefreshIntervals"
                      :key="sec"
                      @click="setAutoRefreshInterval(sec)"
                      class="flex w-full items-center justify-between rounded-md px-3 py-2 text-sm text-gray-700 hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-dark-700"
                    >
                      <span>{{ autoRefreshIntervalLabel(sec) }}</span>
                      <Icon v-if="autoRefreshIntervalSeconds === sec" name="check" size="sm" class="text-primary-500" />
                    </button>
                  </div>
                </div>
              </div>

              <!-- More Tools Dropdown -->
              <div class="relative" ref="accountToolsDropdownRef">
                <button
                  ref="accountToolsTriggerRef"
                  @click="toggleAccountToolsDropdown"
                  class="btn btn-secondary px-2 md:px-3"
                  :title="t('admin.accounts.moreActions')"
                  :aria-expanded="showAccountToolsDropdown"
                >
                  <Icon name="more" size="sm" class="md:mr-1.5" />
                  <span class="hidden md:inline">{{ t('admin.accounts.moreActions') }}</span>
                  <Icon name="chevronDown" size="xs" class="ml-1 hidden md:inline" />
                </button>
                <Teleport to="body">
                  <div
                    v-if="showAccountToolsDropdown"
                    class="fixed z-[9999] origin-top-right overflow-hidden rounded-lg border border-gray-200 bg-white shadow-xl dark:border-dark-700 dark:bg-dark-800"
                    :style="accountToolsDropdownStyle"
                    @click.stop
                  >
                    <div class="overflow-y-auto p-2" :style="{ maxHeight: `${accountToolsDropdownPosition.maxHeight}px` }">
                      <div class="px-2 py-2">
                        <div class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                          {{ t('admin.accounts.dataActions') }}
                        </div>
                      </div>
                      <button class="account-tools-menu-item" @click="openSyncFromCrs">
                        <span class="account-tools-menu-icon bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-300">
                          <Icon name="sync" size="sm" />
                        </span>
                        <span class="flex-1 text-left">{{ t('admin.accounts.syncFromCrs') }}</span>
                      </button>
                      <button class="account-tools-menu-item" @click="openImportData">
                        <span class="account-tools-menu-icon bg-emerald-50 text-emerald-600 dark:bg-emerald-900/30 dark:text-emerald-300">
                          <Icon name="upload" size="sm" />
                        </span>
                        <span class="flex-1 text-left">{{ t('admin.accounts.dataImport') }}</span>
                      </button>
                      <button class="account-tools-menu-item" @click="openExportDataDialogFromMenu">
                        <span class="account-tools-menu-icon bg-violet-50 text-violet-600 dark:bg-violet-900/30 dark:text-violet-300">
                          <Icon name="download" size="sm" />
                        </span>
                        <span class="flex-1 text-left">
                          {{ selIds.length ? t('admin.accounts.dataExportSelected') : t('admin.accounts.dataExport') }}
                        </span>
                        <span
                          v-if="selIds.length"
                          class="rounded-full bg-primary-100 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/40 dark:text-primary-300"
                        >
                          {{ t('admin.accounts.selectedCount', { count: selIds.length }) }}
                        </span>
                      </button>

                      <div class="my-2 border-t border-gray-100 dark:border-dark-700"></div>
                      <div class="px-2 py-2">
                        <div class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                          {{ t('admin.accounts.toolActions') }}
                        </div>
                      </div>
                      <button class="account-tools-menu-item" @click="openErrorPassthrough">
                        <span class="account-tools-menu-icon bg-amber-50 text-amber-600 dark:bg-amber-900/30 dark:text-amber-300">
                          <Icon name="shield" size="sm" />
                        </span>
                        <span class="flex-1 text-left">{{ t('admin.errorPassthrough.title') }}</span>
                      </button>
                      <button class="account-tools-menu-item" @click="openTLSFingerprintProfiles">
                        <span class="account-tools-menu-icon bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-200">
                          <Icon name="lock" size="sm" />
                        </span>
                        <span class="flex-1 text-left">{{ t('admin.tlsFingerprintProfiles.title') }}</span>
                      </button>
                      <button class="account-tools-menu-item" @click="showOpenAIOperations = true; showAccountToolsDropdown = false">
                        <Icon name="chart" size="sm" />
                        <span class="flex-1 text-left">{{ t('admin.openaiOperations.title') }}</span>
                      </button>

                      <div class="my-2 border-t border-gray-100 dark:border-dark-700"></div>
                      <div class="px-2 py-2">
                        <div class="flex items-center justify-between gap-3">
                          <span class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                            {{ t('admin.accounts.viewColumns') }}
                          </span>
                          <Icon name="grid" size="sm" class="text-gray-400" />
                        </div>
                      </div>
                      <div class="grid grid-cols-1 gap-1">
                        <button
                          v-for="col in toggleableColumns"
                          :key="col.key"
                          @click="toggleColumn(col.key)"
                          class="flex w-full items-center justify-between rounded-md px-3 py-2 text-sm text-gray-700 transition-colors hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-dark-700"
                        >
                          <span class="truncate">{{ col.label }}</span>
                          <Icon v-if="isColumnVisible(col.key)" name="check" size="sm" class="text-primary-500" />
                        </button>
                      </div>
                    </div>
                  </div>
                </Teleport>
              </div>
            </template>
          </AccountTableActions>
        </div>
        <div
          v-if="hasPendingListSync"
          class="mt-2 flex items-center justify-between rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:border-amber-700/40 dark:bg-amber-900/20 dark:text-amber-200"
        >
          <span>{{ t('admin.accounts.listPendingSyncHint') }}</span>
          <button
            class="btn btn-secondary px-2 py-1 text-xs"
            @click="syncPendingListChanges"
          >
            {{ t('admin.accounts.listPendingSyncAction') }}
          </button>
        </div>
      </template>
      <template #table>
        <AccountBulkActionsBar
          :selected-ids="selIds"
          :total-results="pagination.total"
          :selecting-all="selectingAllResults"
          :all-results-selected="allResultsSelected"
          @delete="handleBulkDelete"
          @reset-status="handleBulkResetStatus"
          @refresh-token="handleBulkRefreshToken"
          @probe-upstream-billing="handleBulkProbeUpstreamBilling"
          @edit-selected="openBulkEditSelected"
          @edit-filtered="openBulkEditFiltered"
          @clear="clearSelection"
          @select-page="selectPage"
          @select-all-results="handleSelectAllResults"
          @toggle-schedulable="handleBulkToggleSchedulable"
        />
        <div ref="accountTableRef" class="flex min-h-0 flex-1 flex-col overflow-hidden">
        <DataTable
          ref="dataTableRef"
          :columns="cols"
          :data="accounts"
          :loading="loading"
          row-key="id"
          :server-side-sort="true"
          @sort="handleSort"
          default-sort-key="name"
          default-sort-order="asc"
          :sort-storage-key="ACCOUNT_SORT_STORAGE_KEY"
          :estimate-row-height="156"
          :overscan="5"
          :virtualize-threshold="50"
        >
          <template #header-select>
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="allVisibleSelected"
              @click.stop
              @change="toggleSelectAllVisible($event)"
            />
          </template>
          <template #cell-select="{ row }">
            <input type="checkbox" :checked="isSelected(row.id)" @change="toggleSel(row.id)" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
          </template>
          <template #cell-id="{ value }">
            <span class="font-mono text-xs text-gray-500 dark:text-gray-400">#{{ value }}</span>
          </template>
          <template #cell-name="{ row, value }">
            <div class="flex flex-col">
              <HelpTooltip
                v-if="accountHomepageUrl(row)"
                :content="accountHomepageUrl(row)"
                width-class="w-max max-w-sm break-all"
                class="-ml-1 self-start"
              >
                <template #trigger>
                  <a
                    :href="accountHomepageUrl(row)"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="border-b border-dotted border-gray-300 font-medium text-gray-900 dark:border-dark-600 dark:text-white"
                  >
                    {{ value }}
                  </a>
                </template>
              </HelpTooltip>
              <span v-else class="font-medium text-gray-900 dark:text-white">{{ value }}</span>
              <span
                v-if="accountDisplayEmail(row)"
                class="text-xs text-gray-500 dark:text-gray-400 truncate max-w-[200px]"
                :title="accountDisplayEmail(row) + (row.parent_chatgpt_account_id ? ' · ' + row.parent_chatgpt_account_id : '')"
              >
                {{ accountDisplayEmail(row) }}
              </span>
            </div>
          </template>
          <template #cell-notes="{ value }">
            <span v-if="value" :title="value" class="block max-w-xs truncate text-sm text-gray-600 dark:text-gray-300">{{ value }}</span>
            <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
          </template>
          <template #cell-platform_type="{ row }">
            <div class="flex min-w-0 flex-col gap-1">
              <div class="flex flex-wrap items-center gap-1">
                <PlatformTypeBadge :platform="row.platform" :type="row.type"
                  :auth-mode="getOpenAIAuthMode(row)"
                  :plan-type="getAccountPlanType(row)"
                  :privacy-mode="row.extra?.privacy_mode || row.parent_privacy_mode"
                  :subscription-expires-at="row.credentials?.subscription_expires_at || row.parent_subscription_expires_at" />
                <span
                  v-if="getAntigravityTierLabel(row)"
                  :class="['inline-block rounded px-1.5 py-0.5 text-[10px] font-medium', getAntigravityTierClass(row)]"
                >
                  {{ getAntigravityTierLabel(row) }}
                </span>
              </div>
              <div
                v-if="getOpenAICompactMeta(row)"
                :class="[
                  'inline-flex items-center gap-1.5 pl-0.5 text-[11px] font-medium leading-4',
                  getOpenAICompactMeta(row)?.className
                ]"
                :title="getOpenAICompactTitle(row)"
              >
                <span :class="['h-1.5 w-1.5 rounded-full', getOpenAICompactMeta(row)?.dotClass]" />
                <span>{{ getOpenAICompactMeta(row)?.label }}</span>
              </div>
            </div>
          </template>
          <template #cell-workspace="{ row }">
            <!-- r17am：workspace（chatgpt_account_id）短码展示。同色点=同空间，
                 连坐归因一眼可判；悬停看全 ID 与席位数。 -->
            <div v-if="workspaceIdOf(row)" class="flex items-center gap-1.5" :title="workspaceTitle(row)">
              <span :class="['h-2 w-2 flex-shrink-0 rounded-full', workspaceDotClass(row)]" />
              <span class="font-mono text-xs text-gray-600 dark:text-gray-300">{{ workspaceShortId(row) }}</span>
            </div>
            <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
          </template>
          <template #cell-capacity="{ row }">
            <AccountCapacityCell :account="row" />
          </template>
          <template #cell-status="{ row }">
            <div class="flex items-center gap-1.5">
              <AccountStatusIndicator :account="row" @show-temp-unsched="handleShowTempUnsched" />
            </div>
          </template>
          <template #cell-schedulable="{ row }">
            <button @click="handleToggleSchedulable(row)" :disabled="togglingSchedulable === row.id" class="relative inline-flex h-5 w-9 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 dark:focus:ring-offset-dark-800" :class="[row.schedulable ? 'bg-primary-500 hover:bg-primary-600' : 'bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500']" :title="row.schedulable ? t('admin.accounts.schedulableEnabled') : t('admin.accounts.schedulableDisabled')">
              <span class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out" :class="[row.schedulable ? 'translate-x-4' : 'translate-x-0']" />
            </button>
          </template>
          <template #cell-health="{ row }">
            <div v-if="isOpenAIOAuthHealthAccount(row)" class="flex flex-col items-start gap-1">
              <template v-if="accountHealthById[row.id]">
                <button
                  :class="['inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium', healthBadgeClass(accountHealthById[row.id])]"
                  :title="healthBadgeTitle(accountHealthById[row.id])"
                  @click="onHealthBadgeClick(accountHealthById[row.id])"
                >
                  <span :class="['h-1.5 w-1.5 rounded-full', healthDotClass(accountHealthById[row.id])]" />
                  {{ t(`admin.accounts.health.labels.${accountHealthById[row.id].label}`) }}
                </button>
                <!-- task 4.4/4.5：永久复活徽标（转正后常驻血统）+ 签余量读秒
                     芯片（救治中号独有；~ 前缀=插件 fallback 估计值）。 -->
                <div
                  v-if="accountHealthById[row.id].rescued || rescueSignChip(row.id)"
                  class="flex flex-wrap items-center gap-1"
                >
                  <span
                    v-if="accountHealthById[row.id].rescued"
                    class="inline-flex items-center rounded-full border border-emerald-300 bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-emerald-700 dark:border-emerald-500/40 dark:bg-emerald-500/10 dark:text-emerald-300"
                    :title="rescuedChipTitle(accountHealthById[row.id])"
                  >
                    ✿ {{ rescuedChipText(accountHealthById[row.id]) }}
                  </span>
                  <span
                    v-if="rescueSignChip(row.id)"
                    class="inline-flex items-center rounded-full border border-indigo-300 bg-indigo-50 px-1.5 py-0.5 font-mono text-[10px] leading-4 text-indigo-700 dark:border-indigo-500/40 dark:bg-indigo-500/10 dark:text-indigo-300"
                    :title="rescueSignTitle(row.id)"
                  >
                    {{ rescueSignText(row.id) }}
                  </span>
                </div>
                <span
                  v-if="accountHealthById[row.id].last_probe"
                  class="max-w-[11rem] truncate font-mono text-[10px] leading-4 text-gray-500 dark:text-gray-400"
                  :title="lastProbeTitle(accountHealthById[row.id])"
                >
                  {{ lastProbeEvidence(accountHealthById[row.id]) }}
                </span>
                <span v-else class="text-[10px] leading-4 text-gray-400 dark:text-dark-500">
                  {{ t('admin.accounts.health.noProbe') }}
                </span>
              </template>
              <span v-else-if="healthLoading" class="text-[10px] text-gray-400 dark:text-dark-500">…</span>
              <!-- r17an（2026-09-28 用户裁定「被判死的号也要可以主动检测、可以
                   手动启用」）：判死号（pending_replace）双入口——
                   ① 重新启用 = 复活唯一入口（认证针 1 针结业；静置暂停随请求
                      显式解除，专用解暂停不动 schedulable）；
                   ② 主动检测 = 诊断针（只落证据行不动状态机），满足「看看号
                      回来没有」。 -->
              <button
                v-if="accountHealthById[row.id]?.state === 'pending_replace'"
                class="rounded border border-amber-400 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-amber-700 transition-colors hover:bg-amber-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-amber-500 dark:text-amber-300 dark:hover:bg-amber-500/10"
                :disabled="reenablingAccount === row.id"
                :title="accountHealthById[row.id]?.manual_paused ? t('admin.accounts.health.reenablePausedHint') : t('admin.accounts.health.reenableHint')"
                @click="handleReenable(row)"
              >
                {{ reenablingAccount === row.id ? t('admin.accounts.health.reenableRunning') : t('admin.accounts.health.reenable') }}
              </button>
              <!-- task 3.7：判死号第二救援入口——送入实验台（救治区）。与
                   重新启用（认证针人工复活）互补：这条走插件自动救号。已在
                   区（label=rescuing）不重复显示。 -->
              <button
                v-if="accountHealthById[row.id]?.state === 'pending_replace' && accountHealthById[row.id]?.label !== 'rescuing'"
                class="rounded border border-indigo-400 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-indigo-700 transition-colors hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-indigo-500 dark:text-indigo-300 dark:hover:bg-indigo-500/10"
                :disabled="rescuingAccount === row.id"
                :title="t('admin.accounts.health.rescueHint')"
                @click="handleRescue(row)"
              >
                {{ rescuingAccount === row.id ? t('admin.accounts.health.rescueRunning') : t('admin.accounts.health.rescue') }}
              </button>
              <button
                class="rounded border border-gray-300 px-1.5 py-0.5 text-[10px] leading-4 text-gray-600 transition-colors hover:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700"
                :disabled="probingAccounts.has(row.id)"
                @click="handleProbeNow(row)"
              >
                {{ probingAccounts.has(row.id) ? t('admin.accounts.health.probeNowRunning') : t('admin.accounts.health.probeNow') }}
              </button>
            </div>
            <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
          </template>
          <template #cell-today_stats="{ row }">
            <AccountTodayStatsCell
              :stats="todayStatsByAccountId[String(row.id)] ?? null"
              :loading="todayStatsLoading"
              :error="todayStatsError"
            />
          </template>
          <template #cell-groups="{ row }">
            <AccountGroupsCell :groups="row.groups" :max-display="4" />
          </template>
          <template #header-usage="{ column }">
            <div class="flex items-center">
              <span>{{ column.label }}</span>
              <HelpTooltip :content="t('admin.accounts.usageWindowsHint')" width-class="w-72" />
            </div>
          </template>
          <template #cell-usage="{ row }">
            <AccountUsageCell
              :account="row"
              :today-stats="todayStatsByAccountId[String(row.id)] ?? null"
              :today-stats-loading="todayStatsLoading"
              :manual-refresh-token="usageManualRefreshToken"
              :batched-usage="usageBatchByAccountId[String(row.id)] ?? null"
              :batched-usage-error="usageBatchErrorByAccountId[String(row.id)] ?? null"
              :batched-usage-loading="usageBatchLoadingByAccountId[String(row.id)] === true"
              :request-batched-usage="isDesktopViewport ? queueBatchedUsage : null"
              @account-updated="handleAccountUpdated"
              @usage-loaded="handleAccountUsageLoaded(row.id, $event)"
            />
          </template>
          <template #cell-proxy="{ row }">
            <div class="flex flex-col gap-1">
              <div v-if="row.proxy" class="flex items-center gap-2">
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ row.proxy.name }}</span>
                <span v-if="row.proxy.country_code" class="text-xs text-gray-500 dark:text-gray-400">
                  ({{ row.proxy.country_code }})
                </span>
              </div>
              <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
              <div v-if="row.proxy && row.proxy.expires_at" class="flex items-center gap-2 text-xs">
                <span class="text-gray-600 dark:text-gray-300">{{ formatDateTime(row.proxy.expires_at) }}</span>
                <span :class="proxyExpiryBadge(row.proxy)">{{ proxyExpiryText(row.proxy) }}</span>
              </div>
              <div v-if="row.proxy_fallback_origin_id" class="flex items-center gap-1">
                <span class="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200" :title="t('admin.accounts.fallbackActiveTip', { origin: row.proxy_fallback_origin_name })">
                  {{ t('admin.accounts.fallbackActive') }}
                </span>
                <button class="text-xs px-1.5 py-0.5 rounded border border-gray-300 dark:border-dark-600 text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-dark-700" @click="onRevertFallback(row)">{{ t('admin.accounts.revertProxy') }}</button>
              </div>
            </div>
          </template>
          <template #cell-rate_multiplier="{ row }">
            <span class="inline-flex items-center gap-1 text-sm font-mono text-gray-700 dark:text-gray-300">
              <span>{{ formatMultiplier(row.rate_multiplier ?? 1) }}x</span>
              <span
                v-if="row.extra?.upstream_billing_rate_sync_enabled === true"
                class="inline-flex cursor-help text-emerald-600 dark:text-emerald-400"
                :aria-label="t('admin.accounts.upstreamBilling.syncedRateTooltip')"
                :title="t('admin.accounts.upstreamBilling.syncedRateTooltip')"
                data-testid="account-rate-sync-indicator"
              >
                <Icon name="sync" size="xs" />
              </span>
            </span>
          </template>
          <template #header-upstream_billing_rate="{ column }">
            <div class="flex items-center gap-1">
              <span>{{ column.label }}</span>
              <span @click.stop>
                <HelpTooltip :content="t('admin.accounts.upstreamBilling.trustWarning')" width-class="w-80" />
              </span>
            </div>
          </template>
          <template #cell-upstream_billing_rate="{ row }">
            <UpstreamBillingRateCell
              :account="row"
              :global-probe-enabled="upstreamBillingProbeGloballyEnabled"
              :now="upstreamBillingNow"
              :probing="probingUpstreamBilling.has(row.id)"
              @probe="handleProbeUpstreamBilling(row)"
            />
          </template>
          <template #cell-priority="{ value }">
            <span class="text-sm text-gray-700 dark:text-gray-300">{{ value }}</span>
          </template>
          <template #header-scheduler_score="{ column }">
            <div class="flex items-center">
              <span>{{ column.label }}</span>
              <HelpTooltip :content="t('admin.accounts.schedulerScore.hint')" width-class="w-80" />
            </div>
          </template>
          <template #cell-scheduler_score="{ row }">
            <div v-if="getSchedulerScoreRows(row).length" class="flex min-w-[7rem] flex-col gap-0.5 font-mono text-[11px] leading-4">
              <div
                v-for="score in getSchedulerScoreRows(row)"
                :key="String(score.group_id)"
                class="flex items-center gap-1 whitespace-nowrap text-gray-700 dark:text-gray-300"
                :title="`${formatSchedulerScoreGroup(score)} / ${formatSchedulerScore(score.base_score)} / ${formatStickySchedulerScore(score)}`"
              >
                <span class="max-w-[4.75rem] truncate text-gray-500 dark:text-dark-400">{{ formatSchedulerScoreGroup(score) }}</span>
                <span class="text-gray-300 dark:text-gray-600">/</span>
                <span>{{ formatSchedulerScore(score.base_score) }}</span>
                <span class="text-gray-300 dark:text-gray-600">/</span>
                <span class="text-primary-700 dark:text-primary-300">{{ formatStickySchedulerScore(score) }}</span>
              </div>
            </div>
            <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
          </template>
          <template #cell-last_used_at="{ value }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatRelativeTime(value) }}</span>
          </template>
          <template #cell-created_at="{ value }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatDateTime(value) }}</span>
          </template>
          <template #cell-expires_at="{ row, value }">
            <div class="flex flex-col items-start gap-1">
              <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatExpiresAt(value) }}</span>
              <div v-if="isExpired(value) || (row.auto_pause_on_expired && value)" class="flex items-center gap-1">
                <span
                  v-if="isExpired(value)"
                  class="inline-flex items-center rounded-md bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
                >
                  {{ t('admin.accounts.expired') }}
                </span>
                <span
                  v-if="row.auto_pause_on_expired && value"
                  class="inline-flex items-center rounded-md bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
                >
                  {{ t('admin.accounts.autoPauseOnExpired') }}
                </span>
              </div>
            </div>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button @click="handleEdit(row)" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400">
                <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M16.862 4.487l1.687-1.688a1.875 1.875 0 112.652 2.652L10.582 16.07a4.5 4.5 0 01-1.897 1.13L6 18l.8-2.685a4.5 4.5 0 011.13-1.897l8.932-8.931zm0 0L19.5 7.125M18 14v4.75A2.25 2.25 0 0115.75 21H5.25A2.25 2.25 0 013 18.75V8.25A2.25 2.25 0 015.25 6H10" /></svg>
                <span class="text-xs">{{ t('common.edit') }}</span>
              </button>
              <button @click="handleDelete(row)" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400">
                <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M14.74 9l-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 01-2.244 2.077H8.084a2.25 2.25 0 01-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 00-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 013.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 00-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 00-7.5 0" /></svg>
                <span class="text-xs">{{ t('common.delete') }}</span>
              </button>
              <button @click="openMenu(row, $event)" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 dark:hover:bg-dark-700 dark:hover:text-white">
                <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M6.75 12a.75.75 0 11-1.5 0 .75.75 0 011.5 0zM12.75 12a.75.75 0 11-1.5 0 .75.75 0 011.5 0zM18.75 12a.75.75 0 11-1.5 0 .75.75 0 011.5 0z" /></svg>
                <span class="text-xs">{{ t('common.more') }}</span>
              </button>
            </div>
          </template>
        </DataTable>
        </div>
      </template>
      <template #pagination><Pagination v-if="pagination.total > 0" :page="pagination.page" :total="pagination.total" :page-size="pagination.page_size" @update:page="handlePageChange" @update:pageSize="handlePageSizeChange" /></template>
    </TablePageLayout>
    <CreateAccountModal :show="showCreate" :proxies="proxies" :groups="groups" @close="showCreate = false" @created="reload" />
    <EditAccountModal :show="showEdit" :account="edAcc" :proxies="proxies" :groups="groups" @close="showEdit = false" @updated="handleAccountUpdated" />
    <ReAuthAccountModal :show="showReAuth" :account="reAuthAcc" @close="closeReAuthModal" @reauthorized="handleAccountUpdated" />
    <AccountTestModal :show="showTest" :account="testingAcc" @close="closeTestModal" />
    <AccountStatsModal :show="showStats" :account="statsAcc" @close="closeStatsModal" />
    <ScheduledTestsPanel :show="showSchedulePanel" :account-id="scheduleAcc?.id ?? null" :model-options="scheduleModelOptions" @close="closeSchedulePanel" />
    <AccountActionMenu :show="menu.show" :account="menu.acc" :position="menu.pos" @close="menu.show = false" @test="handleTest" @stats="handleViewStats" @schedule="handleSchedule" @duplicate="handleDuplicateAccount" @reauth="handleReAuth" @refresh-token="handleRefresh" @recover-state="handleRecoverState" @reset-quota="handleResetQuota" @set-privacy="handleSetPrivacy" @create-spark-shadow="handleCreateSparkShadow" />
    <SyncFromCrsModal :show="showSync" @close="showSync = false" @synced="reload" />
    <ImportDataModal :show="showImportData" @close="showImportData = false" @imported="handleDataImported" />
    <BulkEditAccountModal
      :show="showBulkEdit"
      :account-ids="selIds"
      :selected-platforms="selPlatforms"
      :selected-types="selTypes"
      :target="bulkEditTarget ?? undefined"
      :proxies="proxies"
      :groups="groups"
      @close="showBulkEdit = false"
      @updated="handleBulkUpdated"
    />
    <TempUnschedStatusModal :show="showTempUnsched" :account="tempUnschedAcc" @close="showTempUnsched = false" @reset="handleTempUnschedReset" />
    <ConfirmDialog :show="showDeleteDialog" :title="t('admin.accounts.deleteAccount')" :message="t('admin.accounts.deleteConfirm', { name: deletingAcc?.name })" :confirm-text="t('common.delete')" :cancel-text="t('common.cancel')" :danger="true" @confirm="confirmDelete" @cancel="showDeleteDialog = false" />
    <ConfirmDialog :show="showProbeConfirm" :title="t('admin.accounts.health.probeNow')" :message="t('admin.accounts.health.rateLimitedConfirm', { name: probingAcc?.name })" @confirm="confirmProbeNow" @cancel="showProbeConfirm = false" />
    <ConfirmDialog :show="showCreateShadowDialog" :title="t('admin.accounts.createSparkShadow')" :message="t('admin.accounts.createSparkShadowConfirm', { name: creatingShadowAcc?.name })" @confirm="confirmCreateSparkShadow" @cancel="showCreateShadowDialog = false" />
    <ConfirmDialog :show="showExportDataDialog" :title="t('admin.accounts.dataExport')" :message="t('admin.accounts.dataExportConfirmMessage')" :confirm-text="t('admin.accounts.dataExportConfirm')" :cancel-text="t('common.cancel')" @confirm="handleExportData" @cancel="showExportDataDialog = false">
      <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
        <input type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" v-model="includeProxyOnExport" />
        <span>{{ t('admin.accounts.dataExportIncludeProxies') }}</span>
      </label>
    </ConfirmDialog>
    <ErrorPassthroughRulesModal :show="showErrorPassthrough" @close="showErrorPassthrough = false" />
    <TLSFingerprintProfilesModal :show="showTLSFingerprintProfiles" @close="showTLSFingerprintProfiles = false" />
    <OpenAIOperationsModal :show="showOpenAIOperations" :groups="groups" :proxies="proxies" @close="showOpenAIOperations = false" />
    <TotpStepUpDialog :controller="accountExportStepUp" />
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted, toRaw, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { adminAPI } from '@/api/admin'
import { useTableLoader } from '@/composables/useTableLoader'
import { useSwipeSelect, type SwipeSelectVirtualContext } from '@/composables/useSwipeSelect'
import { useTableSelection } from '@/composables/useTableSelection'
import { useStepUp, isStepUpBlocked, isStepUpCancelled, stepUpBlockReason } from '@/composables/useStepUp'
import { createOpenAIReenableLifecycle } from '@/composables/openAIReenableLifecycle'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { CreateAccountModal, EditAccountModal, BulkEditAccountModal, SyncFromCrsModal, TempUnschedStatusModal } from '@/components/account'
import AccountTableActions from '@/components/admin/account/AccountTableActions.vue'
import AccountTableFilters from '@/components/admin/account/AccountTableFilters.vue'
import AccountBulkActionsBar from '@/components/admin/account/AccountBulkActionsBar.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import ImportDataModal from '@/components/admin/account/ImportDataModal.vue'
import ReAuthAccountModal from '@/components/admin/account/ReAuthAccountModal.vue'
import AccountTestModal from '@/components/admin/account/AccountTestModal.vue'
import AccountStatsModal from '@/components/admin/account/AccountStatsModal.vue'
import ScheduledTestsPanel from '@/components/admin/account/ScheduledTestsPanel.vue'
import type { SelectOption } from '@/components/common/Select.vue'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import { enqueueUsageRequest } from '@/utils/usageLoadQueue'
import AccountTodayStatsCell from '@/components/account/AccountTodayStatsCell.vue'
import AccountGroupsCell from '@/components/account/AccountGroupsCell.vue'
import AccountCapacityCell from '@/components/account/AccountCapacityCell.vue'
import UpstreamBillingRateCell from '@/components/account/UpstreamBillingRateCell.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import Icon from '@/components/icons/Icon.vue'
import ErrorPassthroughRulesModal from '@/components/admin/ErrorPassthroughRulesModal.vue'
import TLSFingerprintProfilesModal from '@/components/admin/TLSFingerprintProfilesModal.vue'
import OpenAIOperationsModal from '@/components/admin/OpenAIOperationsModal.vue'
import { fetchAllAccountIds } from '@/utils/accountSelection'
import { buildGrokUsageRefreshKey, buildOpenAIUsageRefreshKey } from '@/utils/accountUsageRefresh'
import { formatDateTime, formatRelativeTime } from '@/utils/format'
import { proxyExpiryBadgeClass, proxyExpiryLabelKey } from '@/utils/proxyExpiry'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'
import { sanitizeUrl } from '@/utils/url'
import { getFloatingPanelPosition } from '@/utils/floatingPanel'
import { formatMultiplier } from '@/utils/formatters'
import type { Account, AccountPlatform, AccountSchedulerGroupScore, AccountType, AccountUsageInfo, Proxy as AccountProxy, AdminGroup, WindowStats, ClaudeModel, UpstreamBillingProbeSnapshot } from '@/types'
import type {
  OpenAIAccountHealth,
  OpenAIPluginBridge,
  OpenAIProbeLastEvidence,
} from '@/api/admin/accounts'

const { t } = useI18n()
const appStore = useAppStore()
const authStore = useAuthStore()

const proxies = ref<AccountProxy[]>([])
const groups = ref<AdminGroup[]>([])
const accountTableRef = ref<HTMLElement | null>(null)
const dataTableRef = ref<InstanceType<typeof DataTable> | null>(null)
type AccountBulkEditTarget =
  | {
      mode: 'selected'
      accountIds: number[]
      selectedPlatforms: AccountPlatform[]
      selectedTypes: AccountType[]
    }
  | {
      mode: 'filtered'
      filters: {
        platform?: string
        type?: string
        status?: string
        group?: string
        search?: string
        privacy_mode?: string
        sort_by?: string
        sort_order?: AccountSortOrder
      }
      previewCount: number
      selectedPlatforms: AccountPlatform[]
      selectedTypes: AccountType[]
    }
const selPlatforms = computed<AccountPlatform[]>(() => {
  const platforms = new Set(
    accounts.value
      .filter(a => isSelected(a.id))
      .map(a => a.platform)
  )
  return [...platforms]
})
const selTypes = computed<AccountType[]>(() => {
  const types = new Set(
    accounts.value
      .filter(a => isSelected(a.id))
      .map(a => a.type)
  )
  return [...types]
})
const showCreate = ref(false)
const showEdit = ref(false)
const showSync = ref(false)
const showImportData = ref(false)
const showExportDataDialog = ref(false)
const includeProxyOnExport = ref(true)
const showBulkEdit = ref(false)
const bulkEditTarget = ref<AccountBulkEditTarget | null>(null)
const showTempUnsched = ref(false)
const showDeleteDialog = ref(false)
const showCreateShadowDialog = ref(false)
const showReAuth = ref(false)
const showTest = ref(false)
const showStats = ref(false)
const showErrorPassthrough = ref(false)
const showTLSFingerprintProfiles = ref(false)
const showOpenAIOperations = ref(false)
const edAcc = ref<Account | null>(null)
const tempUnschedAcc = ref<Account | null>(null)
const deletingAcc = ref<Account | null>(null)
const creatingShadowAcc = ref<Account | null>(null)
const reAuthAcc = ref<Account | null>(null)
const testingAcc = ref<Account | null>(null)
const statsAcc = ref<Account | null>(null)
const showSchedulePanel = ref(false)
const scheduleAcc = ref<Account | null>(null)
const scheduleModelOptions = ref<SelectOption[]>([])
const togglingSchedulable = ref<number | null>(null)
const menu = reactive<{show:boolean, acc:Account|null, pos:{top:number, left:number}|null}>({ show: false, acc: null, pos: null })
const exportingData = ref(false)
const probingUpstreamBilling = reactive(new Set<number>())
// Keep the probe gate closed until the backend confirms the global setting.
const upstreamBillingProbeGloballyEnabled = ref(false)
const upstreamBillingNow = ref(Date.now())
const upstreamBillingRateETag = ref<string | null>(null)
const upstreamBillingRateRefreshing = ref(false)
let upstreamBillingRateAbortController: AbortController | null = null
useIntervalFn(() => { upstreamBillingNow.value = Date.now() }, 60_000)

// Account tools dropdown
const showAccountToolsDropdown = ref(false)
const accountToolsDropdownRef = ref<HTMLElement | null>(null)
const accountToolsTriggerRef = ref<HTMLElement | null>(null)
const accountToolsDropdownPosition = reactive({
  top: null as number | null,
  bottom: null as number | null,
  left: 16,
  width: 320,
  maxHeight: 0
})
const accountToolsDropdownStyle = computed(() => ({
  top: accountToolsDropdownPosition.top == null ? 'auto' : `${accountToolsDropdownPosition.top}px`,
  bottom: accountToolsDropdownPosition.bottom == null ? 'auto' : `${accountToolsDropdownPosition.bottom}px`,
  left: `${accountToolsDropdownPosition.left}px`,
  width: `${accountToolsDropdownPosition.width}px`
}))
const hiddenColumns = reactive<Set<string>>(new Set())
// health 列默认显示(相位A 核心交付);老用户若已保存过列布局则尊重其布局,
// 需手动在列选择器里开启。
const DEFAULT_HIDDEN_COLUMNS = ['today_stats', 'proxy', 'notes', 'scheduler_score', 'rate_multiplier']
const HIDDEN_COLUMNS_KEY = 'account-hidden-columns'
// One-time migration: hide scheduler score for existing admins too, because showing it opt-ins to heavy backend scoring.
const HIDDEN_COLUMNS_VERSION_KEY = 'account-hidden-columns-version'
const HIDDEN_COLUMNS_CURRENT_VERSION = 'scheduler-score-hidden-by-default'

// Sorting settings
const ACCOUNT_SORT_STORAGE_KEY = 'account-table-sort'
type AccountSortOrder = 'asc' | 'desc'
type AccountSortState = {
  sort_by: string
  sort_order: AccountSortOrder
}
const ACCOUNT_SORTABLE_KEYS = new Set([
  'id',
  'name',
  'status',
  'schedulable',
  'priority',
  'rate_multiplier',
  'upstream_billing_rate',
  'last_used_at',
  'created_at',
  'expires_at'
])
const loadInitialAccountSortState = (): AccountSortState => {
  const fallback: AccountSortState = { sort_by: 'name', sort_order: 'asc' }
  try {
    const raw = localStorage.getItem(ACCOUNT_SORT_STORAGE_KEY)
    if (!raw) return fallback
    const parsed = JSON.parse(raw) as { key?: string; order?: string }
    const key = typeof parsed.key === 'string' ? parsed.key : ''
    if (!ACCOUNT_SORTABLE_KEYS.has(key)) return fallback
    return {
      sort_by: key,
      sort_order: parsed.order === 'desc' ? 'desc' : 'asc'
    }
  } catch {
    return fallback
  }
}
const sortState = reactive<AccountSortState>(loadInitialAccountSortState())

// Auto refresh settings
const showAutoRefreshDropdown = ref(false)
const autoRefreshDropdownRef = ref<HTMLElement | null>(null)
const AUTO_REFRESH_STORAGE_KEY = 'account-auto-refresh'
const autoRefreshIntervals = [5, 10, 15, 30] as const
const autoRefreshEnabled = ref(false)
const autoRefreshIntervalSeconds = ref<(typeof autoRefreshIntervals)[number]>(30)
const autoRefreshCountdown = ref(0)
const autoRefreshETag = ref<string | null>(null)
const autoRefreshFetching = ref(false)
const AUTO_REFRESH_SILENT_WINDOW_MS = 15000
const autoRefreshSilentUntil = ref(0)
const hasPendingListSync = ref(false)
const todayStatsByAccountId = ref<Record<string, WindowStats>>({})
const todayStatsLoading = ref(false)
const todayStatsError = ref<string | null>(null)
const todayStatsReqSeq = ref(0)
const pendingTodayStatsRefresh = ref(false)
const usageManualRefreshToken = ref(0)

const desktopViewportQuery = '(min-width: 768px)'
const isDesktopViewport = ref(
  typeof window === 'undefined' ? true : window.matchMedia(desktopViewportQuery).matches
)
let desktopViewportMediaQuery: MediaQueryList | null = null
let desktopViewportListener: ((event: MediaQueryListEvent) => void) | null = null

const usageBatchByAccountId = ref<Record<string, AccountUsageInfo | null>>({})
const usageBatchErrorByAccountId = ref<Record<string, string | null>>({})
const usageBatchLoadingByAccountId = ref<Record<string, boolean>>({})
const usageBatchRequestTokenByAccountId = ref<Record<string, number>>({})
const usageBatchCache = new Map<number, { data: AccountUsageInfo; ts: number }>()
const USAGE_BATCH_CACHE_TTL = 5 * 60 * 1000
const pendingUsageBatchIds = new Set<number>()
let usageBatchFlushTimer: ReturnType<typeof setTimeout> | null = null
let queuedUsageBatchForce = false
let usageBatchRequestToken = 0

const invalidateBatchedUsageRequests = () => {
  if (usageBatchFlushTimer !== null) {
    clearTimeout(usageBatchFlushTimer)
    usageBatchFlushTimer = null
  }
  pendingUsageBatchIds.clear()
  queuedUsageBatchForce = false
  usageBatchRequestTokenByAccountId.value = {}
  usageBatchLoadingByAccountId.value = {}
}

watch(isDesktopViewport, (desktop) => {
  if (!desktop) invalidateBatchedUsageRequests()
}, { flush: 'sync' })

const buildDefaultTodayStats = (): WindowStats => ({
  requests: 0,
  tokens: 0,
  cost: 0,
  standard_cost: 0,
  user_cost: 0
})

const accountSupportsBatchUsage = (account: Account) => {
  if (account.platform === 'anthropic') {
    return account.type === 'oauth' || account.type === 'setup-token'
  }
  if (account.platform === 'gemini') return true
  if (account.platform === 'antigravity') return account.type === 'oauth'
  if (account.platform === 'openai') return account.type === 'oauth'
  if (account.platform === 'grok') return account.type === 'oauth'
  return false
}

const setUsageBatchLoading = (accountID: number, loadingState: boolean) => {
  usageBatchLoadingByAccountId.value = {
    ...usageBatchLoadingByAccountId.value,
    [String(accountID)]: loadingState
  }
}

const setUsageBatchState = (accountID: number, usage: AccountUsageInfo | null, error: string | null) => {
  const key = String(accountID)
  usageBatchByAccountId.value = {
    ...usageBatchByAccountId.value,
    [key]: usage
  }
  usageBatchErrorByAccountId.value = {
    ...usageBatchErrorByAccountId.value,
    [key]: error
  }
}

const handleAccountUsageLoaded = (accountID: number, usage: AccountUsageInfo) => {
  if (usageBatchByAccountId.value[String(accountID)] === usage) return
  // A direct (mobile) result supersedes any older queued or in-flight batch.
  pendingUsageBatchIds.delete(accountID)
  usageBatchRequestTokenByAccountId.value[String(accountID)] = ++usageBatchRequestToken
  usageBatchCache.set(accountID, { data: usage, ts: Date.now() })
  setUsageBatchState(accountID, usage, null)
  setUsageBatchLoading(accountID, false)
}

const loadActiveAccountUsage = async (account: Account, token: number) => {
  const key = String(account.id)
  const isCurrent = () => usageBatchRequestTokenByAccountId.value[key] === token
  try {
    const usage = await enqueueUsageRequest(account, () =>
      adminAPI.accounts.getUsage(account.id, 'active', true))
    if (!isCurrent()) return
    usageBatchCache.set(account.id, { data: usage, ts: Date.now() })
    setUsageBatchState(account.id, usage, null)
  } catch (error) {
    if (!isCurrent()) return
    usageBatchErrorByAccountId.value[key] = 'Failed'
    console.error('Failed to load active account usage:', error)
  } finally {
    if (isCurrent()) setUsageBatchLoading(account.id, false)
  }
}

const flushQueuedUsageBatch = async () => {
  usageBatchFlushTimer = null
  const accountIDs = Array.from(pendingUsageBatchIds)
  const force = queuedUsageBatchForce
  pendingUsageBatchIds.clear()
  queuedUsageBatchForce = false

  if (accountIDs.length === 0) return

  const requestTokensByAccount = accountIDs.reduce<Record<string, number>>((acc, accountID) => {
    acc[String(accountID)] = usageBatchRequestTokenByAccountId.value[String(accountID)] ?? 0
    return acc
  }, {})

  try {
    const result = await adminAPI.accounts.getBatchUsage(accountIDs, force)

    const usageMap = result.usage ?? {}
    const errorMap = result.errors ?? {}
    const now = Date.now()
    const nextUsage = { ...usageBatchByAccountId.value }
    const nextErrors = { ...usageBatchErrorByAccountId.value }
    const nextLoading = { ...usageBatchLoadingByAccountId.value }

    for (const accountID of accountIDs) {
      const key = String(accountID)
      if ((usageBatchRequestTokenByAccountId.value[key] ?? 0) !== requestTokensByAccount[key]) {
        continue
      }
      const usage = usageMap[key] ?? null
      const usageError = errorMap[key] ?? (usage ? null : 'Failed')
      nextUsage[key] = usage ?? nextUsage[key] ?? null
      nextErrors[key] = usageError
      nextLoading[key] = false
      if (usage && !usageError) {
        usageBatchCache.set(accountID, { data: usage, ts: now })
      } else {
        usageBatchCache.delete(accountID)
      }
    }

    usageBatchByAccountId.value = nextUsage
    usageBatchErrorByAccountId.value = nextErrors
    usageBatchLoadingByAccountId.value = nextLoading
  } catch (error) {
    const nextErrors = { ...usageBatchErrorByAccountId.value }
    const nextLoading = { ...usageBatchLoadingByAccountId.value }
    for (const accountID of accountIDs) {
      const key = String(accountID)
      if ((usageBatchRequestTokenByAccountId.value[key] ?? 0) !== requestTokensByAccount[key]) {
        continue
      }
      nextErrors[key] = 'Failed'
      nextLoading[key] = false
    }
    usageBatchErrorByAccountId.value = nextErrors
    usageBatchLoadingByAccountId.value = nextLoading
    console.error('Failed to load account usage batch:', error)
  }
}

const queueBatchedUsage = (account: Account, options?: { force?: boolean; source?: 'active' | 'passive' }) => {
  if (!isDesktopViewport.value) return
  if (!accountSupportsBatchUsage(account)) return

  const active = options?.source === 'active' && (
    account.platform === 'anthropic' ||
    (account.platform === 'openai' && account.type === 'oauth')
  )
  const force = options?.force === true || active
  const cacheKey = account.id
  const key = String(cacheKey)

  if (!force && usageBatchLoadingByAccountId.value[key]) return

  if (force) {
    usageBatchCache.delete(cacheKey)
  } else {
    const cached = usageBatchCache.get(cacheKey)
    if (cached && Date.now() - cached.ts < USAGE_BATCH_CACHE_TTL) {
      setUsageBatchState(cacheKey, cached.data, null)
      setUsageBatchLoading(cacheKey, false)
      return
    }
  }

  usageBatchErrorByAccountId.value = {
    ...usageBatchErrorByAccountId.value,
    [key]: null
  }
  usageBatchRequestTokenByAccountId.value = {
    ...usageBatchRequestTokenByAccountId.value,
    [key]: ++usageBatchRequestToken
  }
  setUsageBatchLoading(cacheKey, true)
  if (active) {
    pendingUsageBatchIds.delete(cacheKey)
    return loadActiveAccountUsage(account, usageBatchRequestTokenByAccountId.value[key])
  }
  pendingUsageBatchIds.add(cacheKey)
  queuedUsageBatchForce = queuedUsageBatchForce || force

  if (usageBatchFlushTimer !== null) return
  usageBatchFlushTimer = setTimeout(() => {
    void flushQueuedUsageBatch()
  }, 0)
}

const refreshTodayStatsBatch = async () => {
  // Why this checks both columns:
  // - today_stats column shows dedicated today's metrics.
  // - usage column also embeds today's stats for Key/Bedrock rows.
  // So we only skip fetching when BOTH columns are hidden.
  if (hiddenColumns.has('today_stats') && hiddenColumns.has('usage')) {
    todayStatsLoading.value = false
    todayStatsError.value = null
    return
  }

  const accountIDs = accounts.value.map(account => account.id)
  const reqSeq = ++todayStatsReqSeq.value
  if (accountIDs.length === 0) {
    todayStatsByAccountId.value = {}
    todayStatsError.value = null
    todayStatsLoading.value = false
    return
  }

  todayStatsLoading.value = true
  todayStatsError.value = null

  try {
    const result = await adminAPI.accounts.getBatchTodayStats(accountIDs)
    if (reqSeq !== todayStatsReqSeq.value) return
    const serverStats = result.stats ?? {}
    const nextStats: Record<string, WindowStats> = {}
    for (const accountID of accountIDs) {
      const key = String(accountID)
      nextStats[key] = serverStats[key] ?? buildDefaultTodayStats()
    }
    todayStatsByAccountId.value = nextStats
  } catch (error) {
    if (reqSeq !== todayStatsReqSeq.value) return
    todayStatsError.value = 'Failed'
    console.error('Failed to load account today stats:', error)
  } finally {
    if (reqSeq === todayStatsReqSeq.value) {
      todayStatsLoading.value = false
    }
  }
}

// =============================================================================
// OpenAI 账号健康标签 + 主动检测（相位A，2026-09-21）
// 数据哲学：标签=探针状态机真值+最近一针实测，绝不读滞后的启用状态
// （9/20 批量 401 实战教训）。批量一次查齐；主动检测与调度针完全同构，
// 连点去重（already_flying）；不重置任何配额状态。
// =============================================================================
const accountHealthById = ref<Record<number, OpenAIAccountHealth>>({})
// 救治区插件桥状态（r17ax Phase 2）：随健康快照批量接口带回，救治中标签的
// 离线角标以此为准；无启用插件时为 null。
const pluginBridge = ref<OpenAIPluginBridge | null>(null)
const healthLoading = ref(false)
const healthReqSeq = ref(0)
const probingAccounts = ref(new Set<number>())
// 判死号手动启用中的账号（防重复点击，r17am）。
const reenablingAccount = ref<number | null>(null)
const rescuingAccount = ref<number | null>(null)
const showProbeConfirm = ref(false)
const probingAcc = ref<Account | null>(null)

// task 4.5：签余量读秒——桥 status_json prober.accounts 的签捕获/余量估计
// （原文透传，前端解析）。健康批量刷新时重建 = 桥校准节奏；芯片秒针走
// nowTick。无签（插件尚未捕获）不在表 → 不显芯片。
interface RescueSignInfo {
  capturedAt: number
  estRemainingSec: number | null
  basis: string
}
const rescueSignByAccount = ref<Record<number, RescueSignInfo>>({})
// 秒针：仅在有救治中号且桥给了签数据时走钟（watch 启停，空转零成本）。
const nowTick = ref(Date.now())
let rescueClockTimer: ReturnType<typeof setInterval> | null = null

const parseRescueSignInfo = (bridge: OpenAIPluginBridge | null | undefined): Record<number, RescueSignInfo> => {
  const out: Record<number, RescueSignInfo> = {}
  if (!bridge?.status_json) return out
  try {
    const parsed = JSON.parse(bridge.status_json) as {
      prober?: {
        accounts?: Array<{
          account_id?: number
          sign_captured_at?: string
          estimated_remaining_seconds?: number | null
          estimate_basis?: string
        }>
      }
    }
    for (const acc of parsed.prober?.accounts ?? []) {
      if (!acc || typeof acc.account_id !== 'number' || !acc.sign_captured_at) continue
      const capturedAt = Date.parse(acc.sign_captured_at)
      if (!Number.isFinite(capturedAt)) continue
      out[acc.account_id] = {
        capturedAt,
        estRemainingSec:
          typeof acc.estimated_remaining_seconds === 'number' ? acc.estimated_remaining_seconds : null,
        basis: acc.estimate_basis || 'none'
      }
    }
  } catch {
    // 坏 JSON → 按无签数据渲染（芯片缺席，不炸健康格）。
  }
  return out
}

// 只依赖健康表与签表（accounts 解构在本块之后，勿引用——TDZ）。
const anyRescueSignChipVisible = computed(() => {
  const signs = rescueSignByAccount.value
  for (const health of Object.values(accountHealthById.value)) {
    if (health.rescue && signs[health.account_id]) return true
  }
  return false
})

watch(
  anyRescueSignChipVisible,
  (visible) => {
    if (visible && rescueClockTimer === null) {
      rescueClockTimer = setInterval(() => {
        nowTick.value = Date.now()
      }, 1000)
    } else if (!visible && rescueClockTimer !== null) {
      clearInterval(rescueClockTimer)
      rescueClockTimer = null
    }
  },
  { immediate: true }
)

const fmtSignClock = (seconds: number): string => {
  const total = Math.max(0, Math.floor(seconds))
  const minutes = Math.floor(total / 60)
  if (minutes >= 60) {
    const hours = Math.floor(minutes / 60)
    return `${hours}h${String(minutes % 60).padStart(2, '0')}`
  }
  return `${minutes}:${String(total % 60).padStart(2, '0')}`
}

// 余量以桥 checked_at 为锚扣除流逝（估计值是 Health RPC 时刻算的）。
const rescueSignChip = (accountId: number): { age: string; remain: string | null; approx: boolean } | null => {
  const info = rescueSignByAccount.value[accountId]
  if (!info) return null
  const now = nowTick.value
  const age = fmtSignClock((now - info.capturedAt) / 1000)
  let remain: string | null = null
  if (info.estRemainingSec != null) {
    const anchor = pluginBridge.value?.checked_at ? Date.parse(pluginBridge.value.checked_at) : now
    const elapsed = Number.isFinite(anchor) ? (now - anchor) / 1000 : 0
    remain = fmtSignClock(info.estRemainingSec - elapsed)
  }
  return { age, remain, approx: info.basis !== 'measured' }
}

const rescueSignText = (accountId: number): string => {
  const chip = rescueSignChip(accountId)
  if (!chip) return ''
  const approx = chip.approx ? '~' : ''
  return chip.remain != null
    ? t('admin.accounts.health.signChip', { age: chip.age, remain: approx + chip.remain })
    : t('admin.accounts.health.signAgeChip', { age: chip.age })
}

const rescueSignTitle = (accountId: number): string => {
  const info = rescueSignByAccount.value[accountId]
  if (!info) return ''
  return t('admin.accounts.health.signChipTitle', {
    at: formatDateTime(new Date(info.capturedAt)),
    basis: info.basis
  })
}

// task 4.4：永久复活徽标文案（悬停=复活时间+累计次数）。
const rescuedChipText = (health: OpenAIAccountHealth): string =>
  health.rescued ? t('admin.accounts.health.rescuedBadge', { count: health.rescued.count }) : ''

const rescuedChipTitle = (health: OpenAIAccountHealth): string =>
  health.rescued
    ? t('admin.accounts.health.rescuedTitle', {
        time: formatDateTime(health.rescued.at),
        count: health.rescued.count
      })
    : ''

const isOpenAIOAuthHealthAccount = (row: Account): boolean =>
  row.platform === 'openai' && row.type === 'oauth'

const refreshAccountHealthBatch = async () => {
  if (hiddenColumns.has('health')) return
  const openAIIDs = accounts.value
    .filter(isOpenAIOAuthHealthAccount)
    .map((account) => account.id)
  const reqSeq = ++healthReqSeq.value
  if (openAIIDs.length === 0) {
    accountHealthById.value = {}
    pluginBridge.value = null
    rescueSignByAccount.value = {}
    return
  }
  healthLoading.value = true
  try {
    const result = await adminAPI.accounts.listOpenAIAccountHealth(openAIIDs)
    if (reqSeq !== healthReqSeq.value) return
    const next: Record<number, OpenAIAccountHealth> = {}
    for (const item of result.accounts) next[item.account_id] = item
    accountHealthById.value = next
    pluginBridge.value = result.plugin_bridge ?? null
    rescueSignByAccount.value = parseRescueSignInfo(result.plugin_bridge)
  } catch (error) {
    if (reqSeq !== healthReqSeq.value) return
    console.error('Failed to load account health:', error)
  } finally {
    if (reqSeq === healthReqSeq.value) healthLoading.value = false
  }
}

const healthBadgeClass = (health: OpenAIAccountHealth): string => {
  switch (health.label_color) {
    case 'green':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
    case 'orange':
      return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-300'
    case 'red':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'blue':
      return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    case 'blue-purple':
      // 救治中（r17ax Phase 3.4 标签表）：紫罗兰，与复检中的纯蓝区分。
      return 'bg-violet-100 text-violet-700 dark:bg-violet-900/30 dark:text-violet-300'
    case 'gray-red':
      return 'bg-gray-100 text-red-700 dark:bg-dark-700 dark:text-red-400'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  }
}

const healthDotClass = (health: OpenAIAccountHealth): string => {
  switch (health.label_color) {
    case 'green':
      return 'bg-emerald-500'
    case 'orange':
      return 'bg-orange-500'
    case 'red':
      return 'bg-red-500'
    case 'blue':
      return 'bg-blue-500'
    case 'blue-purple':
      return 'bg-violet-500'
    case 'gray-red':
      return 'bg-red-400'
    default:
      return 'bg-gray-400'
  }
}

const healthBadgeTitle = (health: OpenAIAccountHealth): string => {
  const label = t(`admin.accounts.health.labels.${health.label}`)
  let title = label
  // 救治区注记：连过进度 + 插件侧最近一针（宿主证据行冻结在判死针，
  // 活跃证据在插件——悬停里两本账并排，回应「检测时间没跟上」的观感差）。
  if (health.rescue) {
    title += ` · ${t('admin.accounts.health.rescueProgress', {
      passes: health.rescue.consecutive_passes,
      threshold: health.rescue.graduation_threshold
    })}`
    if (health.rescue.plugin_last_probe_at) {
      const verdict = health.rescue.plugin_last_verdict === 'fail'
        ? t('admin.accounts.health.answerWrong')
        : t('admin.accounts.health.answerCorrect')
      title += ` · ${t('admin.accounts.health.pluginProbeTitle', {
        time: formatDateTime(health.rescue.plugin_last_probe_at),
        verdict
      })}`
    }
  }
  if (!health.clickable) return title
  return `${title} · ${t('admin.accounts.health.clickHint')}`
}

const lastProbeTitle = (health: OpenAIAccountHealth): string => {
  const lp = health.last_probe
  if (!lp) return ''
  const model = lp.mode === 'sol_fallback' || lp.mode === 'sol_fallback_astra' ? 'gpt-5.6-sol' : 'gpt-6-astra'
  return t('admin.accounts.health.lastProbeTitle', {
    time: formatDateTime(lp.at),
    model,
    mode: lp.mode
  })
}

const lastProbeEvidence = (health: OpenAIAccountHealth): string => {
  const lp = health.last_probe
  if (!lp) return ''
  const rt = lp.reasoning_tokens != null ? String(lp.reasoning_tokens) : '—'
  const ts = lp.turn_state_len > 0 ? String(lp.turn_state_len) : '—'
  let answer = t('admin.accounts.health.answerUnknown')
  if (!lp.transport_ok) {
    answer = t('admin.accounts.health.transportInterrupted')
  } else if (lp.answer_correct === true) {
    answer = t('admin.accounts.health.answerCorrect')
  } else if (lp.answer_correct === false) {
    answer = t('admin.accounts.health.answerWrong')
  }
  return t('admin.accounts.health.evidence', {
    age: formatRelativeTime(lp.at),
    rt,
    answer,
    ts
  })
}

// r17am：判死号（pending_replace）标签点击 → 手动启用（认证针 1 针结业）。
// 打票线已整体删除（2026-10-02 r17ax）：pending_replace 是唯一可点标签；
// circuit_open 问题号由半开复检自动恢复，无手动救援动作。
const onHealthBadgeClick = (health: OpenAIAccountHealth) => {
  if (!health.clickable) return
  const row = accounts.value.find((a) => a.id === health.account_id)
  if (!row) return
  if (health.state === 'pending_replace') {
    handleReenable(row)
  }
}

const handleProbeNow = async (row: Account) => {
  if (probingAccounts.value.has(row.id)) return
  const health = accountHealthById.value[row.id]
  // 限流号二次确认（spec 3.4）：会烧一次上游请求额度，且可能吃 429 顺延。
  if (health?.rate_limited) {
    probingAcc.value = row
    showProbeConfirm.value = true
    return
  }
  // r17an：判死号诊断针二次确认——只落证据不复活，但反复测试可能使
  // 惩罚窗升级（社区救援剧本），烧一次上游请求前先说清楚。
  if (health?.state === 'pending_replace') {
    if (!confirm(t('admin.accounts.health.probeDeadConfirm', { name: row.name }))) return
  }
  await runProbeNow(row)
}

const confirmProbeNow = async () => {
  showProbeConfirm.value = false
  if (probingAcc.value) await runProbeNow(probingAcc.value)
}

const runProbeNow = async (row: Account) => {
  if (probingAccounts.value.has(row.id)) return
  probingAccounts.value = new Set(probingAccounts.value).add(row.id)
  try {
    const result = await adminAPI.accounts.triggerOpenAIProbeNow(row.id)
    if (result.retry_after_seconds) {
      appStore.showInfo(t('admin.accounts.health.probeThrottled', { minutes: Math.ceil(result.retry_after_seconds / 60) }))
    } else if (result.already_flying) {
      appStore.showInfo(t('admin.accounts.health.probeAlreadyFlying'))
    } else if (result.probed_now) {
      appStore.showSuccess(t('admin.accounts.health.probeDone'))
    } else {
      appStore.showSuccess(t('admin.accounts.health.probeQueued'))
    }
    // 同步诊断针当场落证据行，立即刷新可见；排队针约 1 分钟后自动刷新可见。
    refreshAccountHealthBatch().catch(() => {})
  } catch (error) {
    appStore.showError(`${t('admin.accounts.health.probeFailed')}: ${extractApiErrorMessage(error)}`)
  } finally {
    const next = new Set(probingAccounts.value)
    next.delete(row.id)
    probingAccounts.value = next
  }
}

// r17am：判死号手动启用。r17an（2026-09-28 用户裁定）：静置暂停不再前置
// 拦截——unpause=true 随请求显式解除刹车（专用解暂停不动 schedulable），
// 确认文案对暂停号说明「将一并解除静置」；后端解除+启用各落审计事件。
const handleReenable = (row: Account) => {
  if (reenablingAccount.value !== null) return
  const health = accountHealthById.value[row.id]
  if (health && health.state !== 'pending_replace') return
  const confirmKey = health?.manual_paused
    ? 'admin.accounts.health.reenablePausedConfirm'
    : 'admin.accounts.health.reenableConfirm'
  if (!confirm(t(confirmKey, { name: row.name }))) return
  reenablingAccount.value = row.id
  const paused = !!health?.manual_paused
  void reenableLifecycle.runRequest({
    accountId: row.id,
    execute: () => adminAPI.accounts.reenableOpenAIAccount(row.id, true),
    getReenabledAt: (result) => new Date(result.reenabled_at).getTime(),
    onSuccess: (result) => {
      if (paused || result.unpaused) {
        appStore.showSuccess(t('admin.accounts.health.reenableUnpaused'))
      }
      appStore.showSuccess(
        t('admin.accounts.health.reenableStarted', { time: formatDateTime(result.next_probe_at) })
      )
      refreshAccountHealthBatch().catch(() => {})
    },
    onError: (error) => {
      appStore.showError(`${t('admin.accounts.health.reenableFailed')}: ${extractApiErrorMessage(error)}`)
    },
    onFinally: () => {
      reenablingAccount.value = null
    },
  })
}

// task 3.7：判死号手动送入救治区（实验台）。与重新启用互补——这条走
// 插件自动救号（绑救治组 + 开调度 + 种子流量）。种子吃凭据级拒绝
// （401/403）时后端已把号退回判死原位，以 409 OPENAI_RESCUE_SEED_AUTH_REJECTED
// 说明；文案指引删号重新授权。clean-passes 阈值展示用后端默认（面板不
// 拉救治区设置；后端设置改动不常见，文案略有出入可接受）。
const RESCUE_CLEAN_PASSES_DEFAULT = 6

const handleRescue = async (row: Account) => {
  if (rescuingAccount.value !== null) return
  const health = accountHealthById.value[row.id]
  if (health && health.state !== 'pending_replace') return
  if (!confirm(t('admin.accounts.health.rescueConfirm', { name: row.name, passes: RESCUE_CLEAN_PASSES_DEFAULT }))) return
  rescuingAccount.value = row.id
  try {
    const result = await adminAPI.accounts.rescueOpenAIAccount(row.id)
    if (result.already_in_lane) {
      appStore.showInfo(t('admin.accounts.health.rescueAlready'))
    } else {
      appStore.showSuccess(t('admin.accounts.health.rescueStarted'))
    }
    refreshAccountHealthBatch().catch(() => {})
  } catch (error) {
    if (extractApiErrorCode(error) === 'OPENAI_RESCUE_SEED_AUTH_REJECTED') {
      appStore.showError(t('admin.accounts.health.rescueAuthRejected'))
    } else {
      appStore.showError(`${t('admin.accounts.health.rescueFailed')}: ${extractApiErrorMessage(error)}`)
    }
  } finally {
    rescuingAccount.value = null
  }
}

// r17an：重新启用结果监视——reenable API 只报「针已排」，认证针异步落地
// （jitter ≤10min + 扫描拍 ≤1min + 针程 ≤2min）。旧体验里答错回死完全
// 静默（9/28 06:13 三号回死毫无感知 → 13:29 删号的直接原因）。这里盯到
// 出结果为止：上岗弹通过，答错弹失败+证据（rt/票长），20s 一拍、15min
// 封顶自动撤岗；无结论针（401/传输）继续盯 5min 重试。
const REENABLE_WATCH_INTERVAL_MS = 20000
const REENABLE_WATCH_DEADLINE_MS = 15 * 60 * 1000

const reenableEvidenceText = (probe: OpenAIProbeLastEvidence | null | undefined): string => {
  if (!probe) return ''
  if (!probe.transport_ok) return t('admin.accounts.health.reenableEvidenceTransport')
  if (probe.answer_correct === false) {
    return t('admin.accounts.health.reenableEvidenceWrong', {
      rt: probe.reasoning_tokens ?? '?',
      ts: probe.turn_state_len || '?',
    })
  }
  return t('admin.accounts.health.reenableEvidenceOther', { status: probe.http_status ?? '?' })
}

const reenableLifecycle = createOpenAIReenableLifecycle<OpenAIAccountHealth>({
  intervalMs: REENABLE_WATCH_INTERVAL_MS,
  deadlineMs: REENABLE_WATCH_DEADLINE_MS,
  fetchHealth: async (accountId) => {
    const [health] = (await adminAPI.accounts.listOpenAIAccountHealth([accountId])).accounts
    return health
  },
  getProbeAt: (health) => health.last_probe ? new Date(health.last_probe.at).getTime() : 0,
  classify: (health) => {
    if (health.state === 'pending_replace') return 'failed'
    if (health.state === 'on_duty' && health.probe_mode === 'normal') return 'passed'
    return null
  },
  onOutcome: (accountId, outcome, health) => {
    const name = accounts.value.find((a) => a.id === accountId)?.name ?? String(accountId)
    if (outcome === 'failed') {
      // 认证针结论=失败：一击退出回判死（r17y），把证据说给人听。
      appStore.showError(t('admin.accounts.health.reenableProbeFailed', {
        name,
        evidence: reenableEvidenceText(health.last_probe),
      }))
      refreshAccountHealthBatch().catch(() => {})
    } else {
      // 认证针通过：上岗+复调度，行数据也该刷新。
      appStore.showSuccess(t('admin.accounts.health.reenableProbePassed', { name }))
      refreshAccountHealthBatch().catch(() => {})
      refreshAccountsIncrementally().catch(() => {})
    }
  },
})

const autoRefreshIntervalLabel = (sec: number) => {
  if (sec === 5) return t('admin.accounts.refreshInterval5s')
  if (sec === 10) return t('admin.accounts.refreshInterval10s')
  if (sec === 15) return t('admin.accounts.refreshInterval15s')
  if (sec === 30) return t('admin.accounts.refreshInterval30s')
  return `${sec}s`
}

const formatSchedulerScore = (value: unknown): string => {
  const num = Number(value)
  if (!Number.isFinite(num)) return '-'
  return num.toFixed(6).replace(/\.?0+$/, '')
}

const formatStickySchedulerScore = (score: AccountSchedulerGroupScore): string => {
  if (!score) return '-'
  if (score.sticky_score_infinity) return '+∞'
  return formatSchedulerScore(score.sticky_score)
}

const getSchedulerScoreRows = (account: Account): AccountSchedulerGroupScore[] => {
  const groupRows = Array.isArray(account.scheduler_scores)
    ? account.scheduler_scores.filter(score => score.group_id != null)
    : []
  if (groupRows.length) return groupRows
  // 未分组账号没有分组维度分数，回退展示后端返回的基础分
  if (account.scheduler_score) {
    return [{ group_id: null, ...account.scheduler_score }]
  }
  return []
}

const formatSchedulerScoreGroup = (score: AccountSchedulerGroupScore): string => {
  if ('group_name' in score && score.group_name) return score.group_name
  if ('group_id' in score && score.group_id != null) return `#${score.group_id}`
  return t('admin.accounts.schedulerScore.ungrouped')
}

const loadSavedColumns = () => {
  try {
    const saved = localStorage.getItem(HIDDEN_COLUMNS_KEY)
    if (saved) {
      const parsed = JSON.parse(saved) as string[]
      parsed.forEach(key => {
        hiddenColumns.add(key)
      })
      // Older saved column layouts may have scheduler_score visible; migrate them to the new safe default once.
      if (localStorage.getItem(HIDDEN_COLUMNS_VERSION_KEY) !== HIDDEN_COLUMNS_CURRENT_VERSION) {
        hiddenColumns.add('scheduler_score')
        localStorage.setItem(HIDDEN_COLUMNS_KEY, JSON.stringify([...hiddenColumns]))
        localStorage.setItem(HIDDEN_COLUMNS_VERSION_KEY, HIDDEN_COLUMNS_CURRENT_VERSION)
      }
    } else {
      DEFAULT_HIDDEN_COLUMNS.forEach(key => {
        hiddenColumns.add(key)
      })
      localStorage.setItem(HIDDEN_COLUMNS_VERSION_KEY, HIDDEN_COLUMNS_CURRENT_VERSION)
    }
  } catch (e) {
    console.error('Failed to load saved columns:', e)
    DEFAULT_HIDDEN_COLUMNS.forEach(key => {
      hiddenColumns.add(key)
    })
  }
}

const saveColumnsToStorage = () => {
  try {
    localStorage.setItem(HIDDEN_COLUMNS_KEY, JSON.stringify([...hiddenColumns]))
    localStorage.setItem(HIDDEN_COLUMNS_VERSION_KEY, HIDDEN_COLUMNS_CURRENT_VERSION)
  } catch (e) {
    console.error('Failed to save columns:', e)
  }
}

const loadSavedAutoRefresh = () => {
  try {
    const saved = localStorage.getItem(AUTO_REFRESH_STORAGE_KEY)
    if (!saved) return
    const parsed = JSON.parse(saved) as { enabled?: boolean; interval_seconds?: number }
    autoRefreshEnabled.value = parsed.enabled === true
    const interval = Number(parsed.interval_seconds)
    if (autoRefreshIntervals.includes(interval as any)) {
      autoRefreshIntervalSeconds.value = interval as any
    }
  } catch (e) {
    console.error('Failed to load saved auto refresh settings:', e)
  }
}

const saveAutoRefreshToStorage = () => {
  try {
    localStorage.setItem(
      AUTO_REFRESH_STORAGE_KEY,
      JSON.stringify({
        enabled: autoRefreshEnabled.value,
        interval_seconds: autoRefreshIntervalSeconds.value
      })
    )
  } catch (e) {
    console.error('Failed to save auto refresh settings:', e)
  }
}

if (typeof window !== 'undefined') {
  loadSavedColumns()
  loadSavedAutoRefresh()
}

const setAutoRefreshEnabled = (enabled: boolean) => {
  autoRefreshEnabled.value = enabled
  saveAutoRefreshToStorage()
  if (enabled) {
    autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
    resumeAutoRefresh()
  } else {
    pauseAutoRefresh()
    autoRefreshCountdown.value = 0
  }
}

const setAutoRefreshInterval = (seconds: (typeof autoRefreshIntervals)[number]) => {
  autoRefreshIntervalSeconds.value = seconds
  saveAutoRefreshToStorage()
  if (autoRefreshEnabled.value) {
    autoRefreshCountdown.value = seconds
  }
}

const toggleColumn = (key: string) => {
  const wasHidden = hiddenColumns.has(key)
  if (hiddenColumns.has(key)) {
    hiddenColumns.delete(key)
  } else {
    hiddenColumns.add(key)
  }
  saveColumnsToStorage()
  if ((key === 'today_stats' || key === 'usage') && wasHidden) {
    refreshTodayStatsBatch().catch((error) => {
      console.error('Failed to load account today stats after showing column:', error)
    })
  }
  if (key === 'health' && wasHidden) {
    refreshAccountHealthBatch().catch((error) => {
      console.error('Failed to load account health after showing column:', error)
    })
  }
  if (key === 'scheduler_score') {
    // The server only returns scheduler scores when this column is visible, so reload the current page immediately.
    syncAccountListDerivedParams()
    load().catch((error) => {
      console.error('Failed to reload accounts after toggling scheduler score column:', error)
    })
  }
}

const isColumnVisible = (key: string) => !hiddenColumns.has(key)
const shouldIncludeSchedulerScore = () => isColumnVisible('scheduler_score')
const syncAccountListDerivedParams = () => {
  // Keep every load path, including auto-refresh and sorting, aligned with the current column visibility.
  const requestParams = params as any
  requestParams.include_scheduler_score = shouldIncludeSchedulerScore() ? '1' : '0'
}

const {
  items: accounts,
  loading,
  params,
  pagination,
  load: baseLoad,
  reload: baseReload,
  debouncedReload: baseDebouncedReload,
  handlePageChange: baseHandlePageChange,
  handlePageSizeChange: baseHandlePageSizeChange
} = useTableLoader<Account, any>({
  fetchFn: adminAPI.accounts.list,
  initialParams: {
    platform: '',
    type: '',
    status: '',
    privacy_mode: '',
    group: '',
    search: '',
    include_scheduler_score: shouldIncludeSchedulerScore() ? '1' : '0',
    sort_by: sortState.sort_by,
    sort_order: sortState.sort_order
  }
})

const {
  selectedSet,
  selectedIds: selIds,
  allVisibleSelected,
  isSelected,
  setSelectedIds,
  select,
  deselect,
  toggle: toggleSel,
  clear: clearSelectedIds,
  removeMany: removeSelectedAccounts,
  toggleVisible,
  selectVisible: selectCurrentPage,
  batchUpdate
} = useTableSelection<Account>({
  rows: accounts,
  getId: (account) => account.id
})

const selectingAllResults = ref(false)
const selectedAllResultIDs = ref<Set<number> | null>(null)
const selectionRequestVersion = ref(0)
const allResultsSelected = computed(() => {
  const snapshot = selectedAllResultIDs.value
  if (!snapshot || snapshot.size === 0 || snapshot.size !== selectedSet.value.size) return false
  return Array.from(snapshot).every(id => selectedSet.value.has(id))
})

const clearSelection = () => {
  selectionRequestVersion.value++
  selectingAllResults.value = false
  selectedAllResultIDs.value = null
  clearSelectedIds()
}

const selectPage = () => {
  selectCurrentPage()
}

const swipeVirtualContext: SwipeSelectVirtualContext = {
  getVirtualizer: () => dataTableRef.value?.virtualizer ?? null,
  getSortedData: () => dataTableRef.value?.sortedData ?? accounts.value,
  getRowId: (row: any) => row.id,
}

useSwipeSelect(accountTableRef, {
  isSelected,
  select,
  deselect,
  batchUpdate
}, swipeVirtualContext)

const resetAutoRefreshCache = () => {
  autoRefreshETag.value = null
  upstreamBillingRateETag.value = null
}

const isFirstLoad = ref(true)

type AccountLoadOptions = {
  refreshTodayStats?: boolean
}

const load = async (options: AccountLoadOptions = {}) => {
  const requestParams = params as any
  syncAccountListDerivedParams()
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = false
  if (isFirstLoad.value) {
    requestParams.lite = '1'
  }
  await baseLoad()
  if (isFirstLoad.value) {
    isFirstLoad.value = false
    delete requestParams.lite
  }
  if (options.refreshTodayStats !== false) await refreshTodayStatsBatch()
  refreshAccountHealthBatch().catch((error) => {
    console.error('Failed to refresh account health:', error)
  })
}

const reload = async () => {
  syncAccountListDerivedParams()
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = false
  await baseReload()
  await refreshTodayStatsBatch()
  refreshAccountHealthBatch().catch((error) => {
    console.error('Failed to refresh account health:', error)
  })
}

const buildUpstreamBillingRateFilters = () => {
  const rawParams = toRaw(params) as Record<string, unknown>
  return {
    platform: typeof rawParams.platform === 'string' ? rawParams.platform : '',
    type: typeof rawParams.type === 'string' ? rawParams.type : '',
    status: typeof rawParams.status === 'string' ? rawParams.status : '',
    group: typeof rawParams.group === 'string' ? rawParams.group : '',
    search: typeof rawParams.search === 'string' ? rawParams.search : '',
    privacy_mode: typeof rawParams.privacy_mode === 'string' ? rawParams.privacy_mode : '',
    sort_by: sortState.sort_by,
    sort_order: sortState.sort_order
  }
}

const sameAccountIDOrder = (left: number[], right: number[]) =>
  left.length === right.length && left.every((id, index) => id === right[index])

const upstreamBillingRateContextKey = () => JSON.stringify({
  page: pagination.page,
  pageSize: pagination.page_size,
  filters: buildUpstreamBillingRateFilters()
})

const applyUpstreamBillingRateSnapshots = async (
  result: NonNullable<Awaited<ReturnType<typeof adminAPI.accounts.getUpstreamBillingRatesWithEtag>>['data']>
) => {
  const nextIDs = result.items.map(item => item.account_id)
  const currentIDs = accounts.value.map(account => account.id)

  // The compact response cannot fill a row that crossed a page boundary.
  // Only that case needs the expensive, full account-list request.
  if (result.total !== pagination.total || !sameAccountIDOrder(nextIDs, currentIDs)) {
    try {
      await load({ refreshTodayStats: false })
    } catch (error) {
      console.error('Failed to reconcile upstream billing sort:', error)
    }
    return
  }

  const itemsByID = new Map(result.items.map(item => [item.account_id, item]))
  let changed = false
  const nextAccounts = accounts.value.map(account => {
    const item = itemsByID.get(account.id)
    if (!item) return account
    const nextSnapshot = item.snapshot ?? null
    const previousSnapshot = account.extra?.upstream_billing_probe ?? null
    if (JSON.stringify(previousSnapshot) === JSON.stringify(nextSnapshot)) return account

    const nextExtra = { ...(account.extra ?? {}) }
    if (nextSnapshot) nextExtra.upstream_billing_probe = nextSnapshot
    else delete nextExtra.upstream_billing_probe
    const nextAccount = {
      ...account,
      ...(typeof nextSnapshot?.synced_rate_multiplier === 'number'
        ? { rate_multiplier: nextSnapshot.synced_rate_multiplier }
        : {}),
      extra: nextExtra
    }
    syncAccountRefs(nextAccount)
    changed = true
    return nextAccount
  })

  if (changed) {
    accounts.value = nextAccounts
    upstreamBillingNow.value = Date.now()
  }
}

const refreshUpstreamBillingRates = async (force = false) => {
  if (upstreamBillingRateRefreshing.value || loading.value || accounts.value.length === 0) return
  if (!force && (
    probingUpstreamBilling.size > 0 ||
    isAnyModalOpen.value ||
    menu.show ||
    showAccountToolsDropdown.value ||
    showAutoRefreshDropdown.value ||
    (typeof document !== 'undefined' && document.hidden)
  )) return

  const controller = new AbortController()
  upstreamBillingRateAbortController = controller
  upstreamBillingRateRefreshing.value = true
  try {
    syncAccountListDerivedParams()
    const requestContextKey = upstreamBillingRateContextKey()
    const result = await adminAPI.accounts.getUpstreamBillingRatesWithEtag(
      pagination.page,
      pagination.page_size,
      buildUpstreamBillingRateFilters(),
      { etag: force ? null : upstreamBillingRateETag.value, signal: controller.signal }
    )
    if (loading.value || requestContextKey !== upstreamBillingRateContextKey()) return
    if (result.etag) upstreamBillingRateETag.value = result.etag
    if (!result.notModified && result.data) await applyUpstreamBillingRateSnapshots(result.data)
  } catch (error) {
    const refreshError = error as { name?: string; code?: string }
    if (refreshError.name !== 'AbortError' && refreshError.name !== 'CanceledError' && refreshError.code !== 'ERR_CANCELED') {
      console.error('Failed to refresh upstream billing rates:', error)
    }
  } finally {
    if (upstreamBillingRateAbortController === controller) upstreamBillingRateAbortController = null
    upstreamBillingRateRefreshing.value = false
  }
}

const refreshUpstreamBillingSortedList = async (force = false) => {
  if (!force && sortState.sort_by !== 'upstream_billing_rate') return
  await refreshUpstreamBillingRates(force)
}

useIntervalFn(() => { void refreshUpstreamBillingRates() }, 5 * 60_000, { immediate: false })

const debouncedReload = () => {
  clearSelection()
  syncAccountListDerivedParams()
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = true
  baseDebouncedReload()
}

const handlePageChange = (page: number) => {
  syncAccountListDerivedParams()
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = true
  baseHandlePageChange(page)
}

const handlePageSizeChange = (size: number) => {
  syncAccountListDerivedParams()
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = true
  baseHandlePageSizeChange(size)
}

const handleSort = (key: string, order: AccountSortOrder) => {
  sortState.sort_by = key
  sortState.sort_order = order
  const requestParams = params as any
  requestParams.sort_by = key
  requestParams.sort_order = order
  syncAccountListDerivedParams()
  pagination.page = 1
  hasPendingListSync.value = false
  resetAutoRefreshCache()
  pendingTodayStatsRefresh.value = true
  load()
}

watch(loading, (isLoading, wasLoading) => {
  if (wasLoading && !isLoading) {
    upstreamBillingNow.value = Date.now()
  }
  if (wasLoading && !isLoading && pendingTodayStatsRefresh.value) {
    pendingTodayStatsRefresh.value = false
    refreshTodayStatsBatch().catch((error) => {
      console.error('Failed to refresh account today stats after table load:', error)
    })
  }
})

watch(accounts, (rows) => {
  const visibleIDs = new Set(rows.map((row) => String(row.id)))
  for (const accountID of pendingUsageBatchIds) {
    if (!visibleIDs.has(String(accountID))) pendingUsageBatchIds.delete(accountID)
  }
  usageBatchByAccountId.value = Object.fromEntries(
    Object.entries(usageBatchByAccountId.value).filter(([key]) => visibleIDs.has(key))
  )
  usageBatchErrorByAccountId.value = Object.fromEntries(
    Object.entries(usageBatchErrorByAccountId.value).filter(([key]) => visibleIDs.has(key))
  )
  usageBatchLoadingByAccountId.value = Object.fromEntries(
    Object.entries(usageBatchLoadingByAccountId.value).filter(([key]) => visibleIDs.has(key))
  )
  usageBatchRequestTokenByAccountId.value = Object.fromEntries(
    Object.entries(usageBatchRequestTokenByAccountId.value).filter(([key]) => visibleIDs.has(key))
  )
})

const isAnyModalOpen = computed(() => {
  return (
    showCreate.value ||
    showEdit.value ||
    showSync.value ||
    showImportData.value ||
    showExportDataDialog.value ||
    showBulkEdit.value ||
    showTempUnsched.value ||
    showDeleteDialog.value ||
    showReAuth.value ||
    showTest.value ||
    showStats.value ||
    showSchedulePanel.value ||
    showErrorPassthrough.value ||
    showTLSFingerprintProfiles.value ||
    showOpenAIOperations.value
  )
})

const enterAutoRefreshSilentWindow = () => {
  autoRefreshSilentUntil.value = Date.now() + AUTO_REFRESH_SILENT_WINDOW_MS
  autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
}

const inAutoRefreshSilentWindow = () => {
  return Date.now() < autoRefreshSilentUntil.value
}

const shouldReplaceAutoRefreshRow = (current: Account, next: Account) => {
  return (
    current.updated_at !== next.updated_at ||
    current.current_concurrency !== next.current_concurrency ||
    current.current_window_cost !== next.current_window_cost ||
    current.active_sessions !== next.active_sessions ||
    current.schedulable !== next.schedulable ||
    current.status !== next.status ||
    current.rate_limit_reset_at !== next.rate_limit_reset_at ||
    current.overload_until !== next.overload_until ||
    current.temp_unschedulable_until !== next.temp_unschedulable_until ||
    buildOpenAIUsageRefreshKey(current) !== buildOpenAIUsageRefreshKey(next) ||
    buildGrokUsageRefreshKey(current) !== buildGrokUsageRefreshKey(next)
  )
}

const syncAccountRefs = (nextAccount: Account) => {
  if (edAcc.value?.id === nextAccount.id) edAcc.value = nextAccount
  if (reAuthAcc.value?.id === nextAccount.id) reAuthAcc.value = nextAccount
  if (tempUnschedAcc.value?.id === nextAccount.id) tempUnschedAcc.value = nextAccount
  if (deletingAcc.value?.id === nextAccount.id) deletingAcc.value = nextAccount
  if (menu.acc?.id === nextAccount.id) menu.acc = nextAccount
}

const mergeAccountsIncrementally = (nextRows: Account[]) => {
  const currentRows = accounts.value
  const currentByID = new Map(currentRows.map(row => [row.id, row]))
  let changed = nextRows.length !== currentRows.length
  const mergedRows = nextRows.map((nextRow) => {
    const currentRow = currentByID.get(nextRow.id)
    if (!currentRow) {
      changed = true
      return nextRow
    }
    if (shouldReplaceAutoRefreshRow(currentRow, nextRow)) {
      changed = true
      syncAccountRefs(nextRow)
      return nextRow
    }
    return currentRow
  })
  if (!changed) {
    for (let i = 0; i < mergedRows.length; i += 1) {
      if (mergedRows[i].id !== currentRows[i]?.id) {
        changed = true
        break
      }
    }
  }
  if (changed) {
    accounts.value = mergedRows
  }
}

const refreshAccountsIncrementally = async () => {
  if (autoRefreshFetching.value) return
  syncAccountListDerivedParams()
  autoRefreshFetching.value = true
  try {
    const result = await adminAPI.accounts.listWithEtag(
      pagination.page,
      pagination.page_size,
      toRaw(params) as {
        platform?: string
        type?: string
        status?: string
        privacy_mode?: string
        group?: string
        search?: string
        sort_by?: string
        sort_order?: AccountSortOrder

      },
      { etag: autoRefreshETag.value }
    )

    if (result.etag) {
      autoRefreshETag.value = result.etag
    }
    if (!result.notModified && result.data) {
      pagination.total = result.data.total || 0
      pagination.pages = result.data.pages || 0
      mergeAccountsIncrementally(result.data.items || [])
      hasPendingListSync.value = false
    }
    upstreamBillingNow.value = Date.now()

    await refreshTodayStatsBatch()
  } catch (error) {
    console.error('Auto refresh failed:', error)
  } finally {
    autoRefreshFetching.value = false
  }
}

const handleManualRefresh = async () => {
  await Promise.all([load(), loadUpstreamBillingProbeGlobalState()])
  // Force usage cells to refetch /usage on explicit user refresh.
  usageManualRefreshToken.value += 1
}

const loadUpstreamBillingProbeGlobalState = async () => {
  try {
    const settings = await adminAPI.accounts.getUpstreamBillingProbeSettings()
    upstreamBillingProbeGloballyEnabled.value = settings?.enabled === true
  } catch (error) {
    upstreamBillingProbeGloballyEnabled.value = false
    console.error('Failed to load upstream billing probe settings:', error)
  }
}

const closeAccountToolsDropdown = () => {
  showAccountToolsDropdown.value = false
}

const updateAccountToolsDropdownPosition = () => {
  const trigger = accountToolsTriggerRef.value
  if (!trigger) return

  const position = getFloatingPanelPosition(
    trigger.getBoundingClientRect(),
    document.documentElement.clientWidth || window.innerWidth,
    window.innerHeight
  )
  Object.assign(accountToolsDropdownPosition, position)
}

const toggleAccountToolsDropdown = () => {
  const nextVisible = !showAccountToolsDropdown.value
  showAutoRefreshDropdown.value = false
  if (nextVisible) updateAccountToolsDropdownPosition()
  showAccountToolsDropdown.value = nextVisible
}

const openSyncFromCrs = () => {
  closeAccountToolsDropdown()
  showSync.value = true
}

const openImportData = () => {
  closeAccountToolsDropdown()
  showImportData.value = true
}

const openExportDataDialogFromMenu = () => {
  closeAccountToolsDropdown()
  openExportDataDialog()
}

const openErrorPassthrough = () => {
  closeAccountToolsDropdown()
  showErrorPassthrough.value = true
}

const openTLSFingerprintProfiles = () => {
  closeAccountToolsDropdown()
  showTLSFingerprintProfiles.value = true
}

const syncPendingListChanges = async () => {
  hasPendingListSync.value = false
  await load()
  // Keep behavior consistent with manual refresh.
  usageManualRefreshToken.value += 1
}

const { pause: pauseAutoRefresh, resume: resumeAutoRefresh } = useIntervalFn(
  async () => {
    if (!autoRefreshEnabled.value) return
    if (document.hidden) return
    if (loading.value || autoRefreshFetching.value) return
    if (isAnyModalOpen.value) return
    if (menu.show || showAccountToolsDropdown.value || showAutoRefreshDropdown.value) return
    if (inAutoRefreshSilentWindow()) {
      autoRefreshCountdown.value = Math.max(
        0,
        Math.ceil((autoRefreshSilentUntil.value - Date.now()) / 1000)
      )
      return
    }

    if (autoRefreshCountdown.value <= 0) {
      autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
      await refreshAccountsIncrementally()
      return
    }

    autoRefreshCountdown.value -= 1
  },
  1000,
  { immediate: false }
)

const GROK_QUOTA_SIGNAL_MAX_AGE_MS = 24 * 60 * 60 * 1000
const GROK_QUOTA_SIGNAL_MAX_FUTURE_SKEW_MS = 5 * 60 * 1000

function firstNonBlankString(...values: unknown[]): string | undefined {
  return values.find((value): value is string => (
    typeof value === 'string' && value.trim().length > 0
  ))
}

function normalizeGrokPlanKey(value: unknown): string {
  if (typeof value !== 'string') return ''
  return value
    .trim()
    .toLowerCase()
    .replace(/[\s_-]+/g, '')
}

function grokPersistedQuotaSnapshot(extra: Record<string, any>): Record<string, any> | undefined {
  const usage = extra.grok_usage_snapshot
  if (usage && typeof usage === 'object' && !Array.isArray(usage)) {
    return usage as Record<string, any>
  }
  const legacy = extra.grok_quota_snapshot
  if (legacy && typeof legacy === 'object' && !Array.isArray(legacy)) {
    return legacy as Record<string, any>
  }
  return undefined
}

function isGrokQuotaTimestampFresh(raw: unknown): boolean {
  const value = String(raw || '').trim()
  if (!value) return false
  const observedAt = Date.parse(value)
  if (!Number.isFinite(observedAt)) return false
  const age = Date.now() - observedAt
  return age <= GROK_QUOTA_SIGNAL_MAX_AGE_MS && age >= -GROK_QUOTA_SIGNAL_MAX_FUTURE_SKEW_MS
}

function isGrok45ResponsesQuotaModel(model: unknown): boolean {
  const value = String(model || '')
    .trim()
    .toLowerCase()
    .replace(/^(x-ai|xai)\//, '')
  return value === 'grok-4.5' || value.startsWith('grok-4.5-')
}

function grokQuotaLooksHeavy(snapshot: Record<string, any> | undefined): boolean {
  const req = Number(snapshot?.requests?.limit ?? 0)
  const tok = Number(snapshot?.tokens?.limit ?? 0)
  return req >= 8300 || tok >= 53_000_000
}

function grok45ResponsesPlanIsHeavy(snapshot: Record<string, any> | undefined): boolean {
  if (!snapshot) return false
  const hint = normalizeGrokPlanKey(snapshot.plan_from_45_responses)
  if (hint === 'supergrokheavy' && isGrokQuotaTimestampFresh(snapshot.plan_from_45_responses_at)) {
    return true
  }
  const observedAt = snapshot.last_headers_seen_at || snapshot.updated_at
  return (
    isGrok45ResponsesQuotaModel(snapshot.model) &&
    isGrokQuotaTimestampFresh(observedAt) &&
    grokQuotaLooksHeavy(snapshot)
  )
}

// JWT / unambiguous credentials outrank snapshots. SuperGrokPro is ambiguous
// (covers SuperGrok and Heavy). 8300/53M only upgrades when the window came
// from grok-4.5 Responses (or a carried 4.5 hint).
function getAccountPlanType(row: any): string | undefined {
  if (!row) return undefined
  if (row.platform === 'grok') {
    const extra = (row.extra || {}) as Record<string, any>
    const billing = extra.grok_billing_snapshot as Record<string, any> | undefined
    const usage = extra.grok_usage_snapshot as Record<string, any> | undefined
    const legacyQuota = extra.grok_quota_snapshot as Record<string, any> | undefined
    const quota = grokPersistedQuotaSnapshot(extra)
    const cred = firstNonBlankString(row.credentials?.subscription_tier)
    const credKey = normalizeGrokPlanKey(cred)
    if (credKey && credKey !== 'supergrokpro') {
      return cred
    }
    if (
      grok45ResponsesPlanIsHeavy(quota) &&
      (credKey === 'supergrokpro' ||
        normalizeGrokPlanKey(billing?.plan) === 'supergrok' ||
        normalizeGrokPlanKey(billing?.plan) === 'supergrokpro')
    ) {
      return 'SuperGrok Heavy'
    }
    if (credKey === 'supergrokpro') {
      return firstNonBlankString(billing?.plan) || 'SuperGrok'
    }
    return firstNonBlankString(
      billing?.plan,
      usage?.subscription_tier,
      legacyQuota?.subscription_tier,
      extra.subscription_tier,
      row.credentials?.plan_type,
      row.parent_plan_type
    )
  }
  return firstNonBlankString(row.credentials?.plan_type, row.parent_plan_type)
}

function getOpenAIAuthMode(row: any): string | undefined {
  if (!row || row.platform !== 'openai' || row.type !== 'oauth') return undefined
  const authMode = row.credentials?.auth_mode
  return typeof authMode === 'string' && authMode.trim() ? authMode : undefined
}

// =============================================================================
// workspace 列辅助（r17am）：chatgpt_account_id 非敏感凭据（不在脱敏清单，
// row.credentials 直达），短码+8 色哈希点做肉眼分组。影子号回退母号空间。
// =============================================================================
const WORKSPACE_DOT_CLASSES = [
  'bg-rose-500',
  'bg-orange-500',
  'bg-amber-500',
  'bg-emerald-500',
  'bg-teal-500',
  'bg-blue-500',
  'bg-violet-500',
  'bg-pink-500'
] as const

function workspaceIdOf(row: any): string {
  if (!row || row.platform !== 'openai') return ''
  const own = row.credentials?.chatgpt_account_id
  if (typeof own === 'string' && own.trim()) return own.trim()
  const parent = row.parent_chatgpt_account_id
  return typeof parent === 'string' && parent.trim() ? parent.trim() : ''
}

function workspaceShortId(row: any): string {
  const id = workspaceIdOf(row)
  return id ? id.slice(-8) : ''
}

function workspaceDotClass(row: any): string {
  const id = workspaceIdOf(row)
  if (!id) return 'bg-gray-400'
  let hash = 0
  for (let i = 0; i < id.length; i++) {
    hash = (hash * 31 + id.charCodeAt(i)) >>> 0
  }
  return WORKSPACE_DOT_CLASSES[hash % WORKSPACE_DOT_CLASSES.length]
}

function workspaceTitle(row: any): string {
  const id = workspaceIdOf(row)
  if (!id) return ''
  const seats = Number(row.extra?.seat_count)
  const seatText = Number.isFinite(seats) && seats > 0
    ? t('admin.accounts.workspaceTitle', { count: seats })
    : ''
  return seatText ? `${id} · ${seatText}` : id
}

// Antigravity 订阅等级辅助函数
function getAntigravityTierFromRow(row: any): string | null {
  if (row.platform !== 'antigravity') return null
  const extra = row.extra as Record<string, unknown> | undefined
  if (!extra) return null
  const lca = extra.load_code_assist as Record<string, unknown> | undefined
  if (!lca) return null
  const paid = lca.paidTier as Record<string, unknown> | undefined
  if (paid && typeof paid.id === 'string') return paid.id
  const current = lca.currentTier as Record<string, unknown> | undefined
  if (current && typeof current.id === 'string') return current.id
  return null
}

function getAntigravityTierLabel(row: any): string | null {
  const tier = getAntigravityTierFromRow(row)
  switch (tier) {
    case 'free-tier': return t('admin.accounts.tier.free')
    case 'g1-pro-tier': return t('admin.accounts.tier.pro')
    case 'g1-ultra-tier': return t('admin.accounts.tier.ultra')
    default: return null
  }
}

// 账号显示邮箱:优先账号自身(extra/credentials),影子账号回退母账号 parent_email。
// 供名称单元格 v-if/标题/文本三处共用,避免同一回退链在模板里重复三次。
function accountDisplayEmail(row: any): string {
  return row.extra?.email_address || row.extra?.email || row.credentials?.email || row.parent_email || ''
}

function accountHomepageUrl(row: Account): string {
  if (row.type !== 'apikey' || typeof row.credentials?.base_url !== 'string') return ''
  const baseUrl = sanitizeUrl(row.credentials.base_url)
  return baseUrl ? new URL(baseUrl).origin : ''
}

type OpenAICompactBadgeState = 'active' | 'blocked' | 'auto'

function getOpenAICompactState(row: any): OpenAICompactBadgeState | null {
  if (row.platform !== 'openai' || (row.type !== 'oauth' && row.type !== 'apikey')) return null
  const extra = row.extra as Record<string, unknown> | undefined
  const mode = typeof extra?.openai_compact_mode === 'string' ? extra.openai_compact_mode : 'auto'
  if (mode === 'force_on') return 'active'
  if (mode === 'force_off') return 'blocked'
  if (typeof extra?.openai_compact_supported === 'boolean') {
    return extra.openai_compact_supported ? 'active' : 'blocked'
  }
  return 'auto'
}

function getOpenAICompactMeta(row: any): { label: string; className: string; dotClass: string } | null {
  const state = getOpenAICompactState(row)
  if (!state) return null
  switch (state) {
    case 'active':
      return {
        label: t('admin.accounts.openai.compactSupported'),
        className: 'text-emerald-600 dark:text-emerald-300',
        dotClass: 'bg-emerald-500 shadow-[0_0_0_2px_rgba(16,185,129,0.14)]'
      }
    case 'blocked':
      return {
        label: t('admin.accounts.openai.compactUnsupported'),
        className: 'text-rose-600 dark:text-rose-300',
        dotClass: 'bg-rose-500 shadow-[0_0_0_2px_rgba(244,63,94,0.14)]'
      }
    case 'auto':
      return {
        label: t('admin.accounts.openai.compactAuto'),
        className: 'text-slate-500 dark:text-slate-400',
        dotClass: 'bg-slate-300 dark:bg-slate-500'
      }
  }
}

function getOpenAICompactTitle(row: any): string {
  const extra = row.extra as Record<string, unknown> | undefined
  const checkedAt = typeof extra?.openai_compact_checked_at === 'string' ? extra.openai_compact_checked_at : ''
  const label = getOpenAICompactMeta(row)?.label || ''
  if (!checkedAt) return label
  return `${label} | ${t('admin.accounts.openai.compactLastChecked')}: ${formatDateTime(new Date(checkedAt))}`
}

function getAntigravityTierClass(row: any): string {
  const tier = getAntigravityTierFromRow(row)
  switch (tier) {
    case 'free-tier': return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
    case 'g1-pro-tier': return 'bg-blue-100 text-blue-600 dark:bg-blue-900/40 dark:text-blue-300'
    case 'g1-ultra-tier': return 'bg-purple-100 text-purple-600 dark:bg-purple-900/40 dark:text-purple-300'
    default: return ''
  }
}

// All available columns
const allColumns = computed(() => {
  const c = [
    { key: 'select', label: '', sortable: false },
    { key: 'name', label: t('admin.accounts.columns.name'), sortable: true },
    { key: 'id', label: t('admin.accounts.columns.id'), sortable: true },
    { key: 'platform_type', label: t('admin.accounts.columns.platformType'), sortable: false },
    // r17am：workspace 列——chatgpt_account_id 短码+哈希色点，供连坐案
    // （1187/1192-1195）肉眼分组归因。数据已在 row.credentials，纯前端列。
    { key: 'workspace', label: t('admin.accounts.columns.workspace'), sortable: false },
    { key: 'capacity', label: t('admin.accounts.columns.capacity'), sortable: false },
    { key: 'status', label: t('admin.accounts.columns.status'), sortable: true },
    { key: 'schedulable', label: t('admin.accounts.columns.schedulable'), sortable: true },
    { key: 'health', label: t('admin.accounts.columns.health'), sortable: false },
    { key: 'today_stats', label: t('admin.accounts.columns.todayStats'), sortable: false }
  ]
  if (!authStore.isSimpleMode) {
    c.push({ key: 'groups', label: t('admin.accounts.columns.groups'), sortable: false })
  }
  c.push({ key: 'usage', label: t('admin.accounts.columns.usageWindows'), sortable: false })
  c.push(
    { key: 'proxy', label: t('admin.accounts.columns.proxy'), sortable: false },
    { key: 'priority', label: t('admin.accounts.columns.priority'), sortable: true },
    { key: 'scheduler_score', label: t('admin.accounts.columns.schedulerScore'), sortable: false },
    { key: 'rate_multiplier', label: t('admin.accounts.columns.billingRateMultiplier'), sortable: true },
    { key: 'upstream_billing_rate', label: t('admin.accounts.columns.upstreamBillingRate'), sortable: true },
    { key: 'last_used_at', label: t('admin.accounts.columns.lastUsed'), sortable: true },
    { key: 'created_at', label: t('admin.accounts.columns.createdAt'), sortable: true },
    { key: 'expires_at', label: t('admin.accounts.columns.expiresAt'), sortable: true },
    { key: 'notes', label: t('admin.accounts.columns.notes'), sortable: false },
    { key: 'actions', label: t('admin.accounts.columns.actions'), sortable: false }
  )
  return c
})

// Columns that can be toggled (exclude select, name, and actions)
const toggleableColumns = computed(() =>
  allColumns.value.filter(col => col.key !== 'select' && col.key !== 'name' && col.key !== 'actions')
)

// Filtered columns based on visibility
const cols = computed(() =>
  allColumns.value.filter(col =>
    col.key === 'select' || col.key === 'name' || col.key === 'actions' || !hiddenColumns.has(col.key)
  )
)

const handleEdit = (a: Account) => { edAcc.value = a; showEdit.value = true }
const openMenu = (a: Account, e: MouseEvent) => {
  menu.acc = a

  const target = e.currentTarget as HTMLElement
  if (target) {
    const rect = target.getBoundingClientRect()
    const menuWidth = 200
    const menuHeight = 240
    const padding = 8
    const viewportWidth = window.innerWidth
    const viewportHeight = window.innerHeight

    let left: number
    let top: number

    if (viewportWidth < 768) {
      // 居中显示,水平位置
      left = Math.max(padding, Math.min(
        rect.left + rect.width / 2 - menuWidth / 2,
        viewportWidth - menuWidth - padding
      ))

      // 优先显示在按钮下方
      top = rect.bottom + 4

      // 如果下方空间不够,显示在上方
      if (top + menuHeight > viewportHeight - padding) {
        top = rect.top - menuHeight - 4
        // 如果上方也不够,就贴在视口顶部
        if (top < padding) {
          top = padding
        }
      }
    } else {
      left = Math.max(padding, Math.min(
        e.clientX - menuWidth,
        viewportWidth - menuWidth - padding
      ))
      top = e.clientY
      if (top + menuHeight > viewportHeight - padding) {
        top = viewportHeight - menuHeight - padding
      }
    }

    menu.pos = { top, left }
  } else {
    menu.pos = { top: e.clientY, left: e.clientX - 200 }
  }

  menu.show = true
}
const toggleSelectAllVisible = (event: Event) => {
  const target = event.target as HTMLInputElement
  toggleVisible(target.checked)
}
const handleBulkDelete = async () => {
  const accountIds = [...selIds.value]
  if (!confirm(t('admin.accounts.bulkActions.confirmDelete', { count: accountIds.length }))) return
  try {
    const result = await adminAPI.accounts.batchDelete(accountIds)
    if (result.failed > 0) {
      appStore.showError(t('admin.accounts.bulkActions.partialSuccess', {
        success: result.success,
        failed: result.failed
      }))
      setSelectedIds(result.failed_ids?.length ? result.failed_ids : accountIds)
    } else {
      appStore.showSuccess(t('admin.accounts.bulkActions.deleteSuccess', { count: result.success }))
      clearSelection()
    }
    await reload()
  } catch (error) {
    console.error('Failed to bulk delete accounts:', error)
    appStore.showError(String(error))
  }
}
const handleBulkResetStatus = async () => {
  if (!confirm(t('common.confirm'))) return
  try {
    const result = await adminAPI.accounts.batchClearError(selIds.value)
    if (result.failed > 0) {
      appStore.showError(t('admin.accounts.bulkActions.partialSuccess', { success: result.success, failed: result.failed }))
    } else {
      appStore.showSuccess(t('admin.accounts.bulkActions.resetStatusSuccess', { count: result.success }))
      clearSelection()
    }
    reload()
  } catch (error) {
    console.error('Failed to bulk reset status:', error)
    appStore.showError(String(error))
  }
}
const handleBulkRefreshToken = async () => {
  if (!confirm(t('common.confirm'))) return
  try {
    const result = await adminAPI.accounts.batchRefresh(selIds.value)
    if (result.failed > 0) {
      appStore.showError(t('admin.accounts.bulkActions.partialSuccess', { success: result.success, failed: result.failed }))
    } else {
      appStore.showSuccess(t('admin.accounts.bulkActions.refreshTokenSuccess', { count: result.success }))
      clearSelection()
    }
    reload()
  } catch (error) {
    console.error('Failed to bulk refresh token:', error)
    appStore.showError(String(error))
  }
}
const handleBulkProbeUpstreamBilling = async () => {
  const accountIDs = [...selIds.value]
  if (accountIDs.length === 0) {
    appStore.showError(t('admin.accounts.upstreamBilling.noEligibleAccounts'))
    return
  }
  if (accountIDs.length > 20) {
    appStore.showError(t('admin.accounts.upstreamBilling.batchLimit'))
    return
  }
  accountIDs.forEach(id => probingUpstreamBilling.add(id))
  try {
    const results = await adminAPI.accounts.probeUpstreamBillingBatch(accountIDs)
    let patched = false
    results.forEach(result => {
      if (result.snapshot) {
        patchUpstreamBillingSnapshot(result.account_id, result.snapshot)
        patched = true
      }
    })
    if (patched) await refreshAccountsAfterUpstreamBillingProbe()
    const failed = results.filter(result => result.error).length
    if (failed > 0) {
      appStore.showError(t('admin.accounts.upstreamBilling.batchPartial', { success: results.length - failed, failed }))
    } else {
      appStore.showSuccess(t('admin.accounts.upstreamBilling.batchCompleted', { count: results.length }))
    }
  } catch (error) {
    console.error('Failed to probe upstream billing in batch:', error)
    appStore.showError(extractApiErrorMessage(error, t('admin.accounts.upstreamBilling.probeFailed')))
  } finally {
    accountIDs.forEach(id => probingUpstreamBilling.delete(id))
  }
}
const updateSchedulableInList = (accountIds: number[], schedulable: boolean) => {
  if (accountIds.length === 0) return
  const idSet = new Set(accountIds)
  accounts.value = accounts.value.map((account) => (idSet.has(account.id) ? { ...account, schedulable } : account))
}
const normalizeBulkSchedulableResult = (
  result: {
    success?: number
    failed?: number
    success_ids?: number[]
    failed_ids?: number[]
    results?: Array<{ account_id: number; success: boolean }>
  },
  accountIds: number[]
) => {
  const responseSuccessIds = Array.isArray(result.success_ids) ? result.success_ids : []
  const responseFailedIds = Array.isArray(result.failed_ids) ? result.failed_ids : []
  if (responseSuccessIds.length > 0 || responseFailedIds.length > 0) {
    return {
      successIds: responseSuccessIds,
      failedIds: responseFailedIds,
      successCount: typeof result.success === 'number' ? result.success : responseSuccessIds.length,
      failedCount: typeof result.failed === 'number' ? result.failed : responseFailedIds.length,
      hasIds: true,
      hasCounts: true
    }
  }

  const results = Array.isArray(result.results) ? result.results : []
  if (results.length > 0) {
    const successIds = results.filter(item => item.success).map(item => item.account_id)
    const failedIds = results.filter(item => !item.success).map(item => item.account_id)
    return {
      successIds,
      failedIds,
      successCount: typeof result.success === 'number' ? result.success : successIds.length,
      failedCount: typeof result.failed === 'number' ? result.failed : failedIds.length,
      hasIds: true,
      hasCounts: true
    }
  }

  const hasExplicitCounts = typeof result.success === 'number' || typeof result.failed === 'number'
  const successCount = typeof result.success === 'number' ? result.success : 0
  const failedCount = typeof result.failed === 'number' ? result.failed : 0
  if (hasExplicitCounts && failedCount === 0 && successCount === accountIds.length && accountIds.length > 0) {
    return {
      successIds: accountIds,
      failedIds: [],
      successCount,
      failedCount,
      hasIds: true,
      hasCounts: true
    }
  }

  return {
    successIds: [],
    failedIds: [],
    successCount,
    failedCount,
    hasIds: false,
    hasCounts: hasExplicitCounts
  }
}
const handleBulkToggleSchedulable = async (schedulable: boolean) => {
  const accountIds = [...selIds.value]
  try {
    const result = await adminAPI.accounts.bulkUpdate(accountIds, { schedulable })
    const { successIds, failedIds, successCount, failedCount, hasIds, hasCounts } = normalizeBulkSchedulableResult(result, accountIds)
    if (!hasIds && !hasCounts) {
      appStore.showError(t('admin.accounts.bulkSchedulableResultUnknown'))
      setSelectedIds(accountIds)
      load().catch((error) => {
        console.error('Failed to refresh accounts:', error)
      })
      return
    }
    if (successIds.length > 0) {
      updateSchedulableInList(successIds, schedulable)
    }
    if (successCount > 0 && failedCount === 0) {
      const message = schedulable
        ? t('admin.accounts.bulkSchedulableEnabled', { count: successCount })
        : t('admin.accounts.bulkSchedulableDisabled', { count: successCount })
      appStore.showSuccess(message)
    }
    if (failedCount > 0) {
      const message = hasCounts || hasIds
        ? t('admin.accounts.bulkSchedulablePartial', { success: successCount, failed: failedCount })
        : t('admin.accounts.bulkSchedulableResultUnknown')
      appStore.showError(message)
      setSelectedIds(failedIds.length > 0 ? failedIds : accountIds)
    } else {
      if (hasIds) clearSelection()
      else setSelectedIds(accountIds)
    }
  } catch (error) {
    console.error('Failed to bulk toggle schedulable:', error)
    appStore.showError(t('common.error'))
  }
}
const buildBulkEditFilterSnapshot = () => {
  const rawParams = toRaw(params) as Record<string, unknown>
  const sortOrder: AccountSortOrder = rawParams.sort_order === 'desc' ? 'desc' : 'asc'
  return {
    platform: typeof rawParams.platform === 'string' ? rawParams.platform : '',
    type: typeof rawParams.type === 'string' ? rawParams.type : '',
    status: typeof rawParams.status === 'string' ? rawParams.status : '',
    group: typeof rawParams.group === 'string' ? rawParams.group : '',
    search: typeof rawParams.search === 'string' ? rawParams.search : '',
    privacy_mode: typeof rawParams.privacy_mode === 'string' ? rawParams.privacy_mode : '',
    sort_by: typeof rawParams.sort_by === 'string' ? rawParams.sort_by : '',
    sort_order: sortOrder
  }
}

const handleSelectAllResults = async () => {
  if (selectingAllResults.value || pagination.total === 0) return

  const requestVersion = ++selectionRequestVersion.value
  const filters = buildBulkEditFilterSnapshot()
  selectingAllResults.value = true
  try {
    const ids = await fetchAllAccountIds(
      (page, pageSize, requestFilters) => adminAPI.accounts.list(page, pageSize, requestFilters),
      filters
    )
    if (requestVersion !== selectionRequestVersion.value) return

    setSelectedIds(ids)
    selectedAllResultIDs.value = new Set(ids)
  } catch (error) {
    if (requestVersion !== selectionRequestVersion.value) return
    console.error('Failed to select all account results:', error)
    appStore.showError(t('admin.accounts.bulkActions.selectAllFailed'))
  } finally {
    if (requestVersion === selectionRequestVersion.value) {
      selectingAllResults.value = false
    }
  }
}

const collectSelectionMetadata = (rows: Account[]) => {
  const selectedPlatforms = Array.from(new Set(rows.map(account => account.platform)))
  const selectedTypes = Array.from(new Set(rows.map(account => account.type)))
  return { selectedPlatforms, selectedTypes }
}

const openBulkEditSelected = () => {
  bulkEditTarget.value = {
    mode: 'selected',
    accountIds: [...selIds.value],
    selectedPlatforms: [...selPlatforms.value],
    selectedTypes: [...selTypes.value]
  }
  showBulkEdit.value = true
}

const openBulkEditFiltered = async () => {
  const filters = buildBulkEditFilterSnapshot()
  const preview = await adminAPI.accounts.list(1, 100, filters)
  const { selectedPlatforms, selectedTypes } = collectSelectionMetadata(preview.items)
  bulkEditTarget.value = {
    mode: 'filtered',
    filters,
    previewCount: preview.total,
    selectedPlatforms,
    selectedTypes
  }
  showBulkEdit.value = true
}

const handleBulkUpdated = () => {
  showBulkEdit.value = false
  bulkEditTarget.value = null
  clearSelection()
  reload()
}
const handleDataImported = () => { showImportData.value = false; reload() }
const ACCOUNT_UNGROUPED_GROUP_QUERY_VALUE = 'ungrouped'
const ACCOUNT_PRIVACY_MODE_UNSET_QUERY_VALUE = '__unset__'
const buildAccountQueryFilters = () => ({
  platform: params.platform || '',
  type: params.type || '',
  status: params.status || '',
  group: params.group || '',
  privacy_mode: params.privacy_mode || '',
  search: params.search || '',
  sort_by: sortState.sort_by,
  sort_order: sortState.sort_order
})
const accountMatchesCurrentFilters = (account: Account) => {
  const filters = buildAccountQueryFilters()
  if (filters.platform && account.platform !== filters.platform) return false
  if (filters.type && account.type !== filters.type) return false
  if (filters.status) {
    const now = Date.now()
    const rateLimitResetAt = account.rate_limit_reset_at ? new Date(account.rate_limit_reset_at).getTime() : Number.NaN
    const isRateLimited = Number.isFinite(rateLimitResetAt) && rateLimitResetAt > now
    const tempUnschedUntil = account.temp_unschedulable_until ? new Date(account.temp_unschedulable_until).getTime() : Number.NaN
    const isTempUnschedulable = Number.isFinite(tempUnschedUntil) && tempUnschedUntil > now

    if (filters.status === 'active') {
      if (account.status !== 'active' || isRateLimited || isTempUnschedulable || !account.schedulable) return false
    } else if (filters.status === 'rate_limited') {
      if (account.status !== 'active' || !isRateLimited || isTempUnschedulable) return false
    } else if (filters.status === 'temp_unschedulable') {
      if (account.status !== 'active' || !isTempUnschedulable) return false
    } else if (filters.status === 'unschedulable') {
      if (account.status !== 'active' || account.schedulable || isRateLimited || isTempUnschedulable) return false
    } else if (account.status !== filters.status) {
      return false
    }
  }
  if (filters.group) {
    const groupIds = account.group_ids ?? account.groups?.map((group) => group.id) ?? []
    if (filters.group === ACCOUNT_UNGROUPED_GROUP_QUERY_VALUE) {
      if (groupIds.length > 0) return false
    } else if (!groupIds.includes(Number(filters.group))) {
      return false
    }
  }
  const privacyMode = typeof account.extra?.privacy_mode === 'string' ? account.extra.privacy_mode : ''
  if (filters.privacy_mode) {
    if (filters.privacy_mode === ACCOUNT_PRIVACY_MODE_UNSET_QUERY_VALUE) {
      if (privacyMode.trim() !== '') return false
    } else if (privacyMode !== filters.privacy_mode) {
      return false
    }
  }
  const search = String(filters.search || '').trim().toLowerCase()
  if (search && !account.name.toLowerCase().includes(search)) return false
  return true
}
const mergeRuntimeFields = (oldAccount: Account, updatedAccount: Account): Account => ({
  ...updatedAccount,
  current_concurrency: updatedAccount.current_concurrency ?? oldAccount.current_concurrency,
  current_window_cost: updatedAccount.current_window_cost ?? oldAccount.current_window_cost,
  active_sessions: updatedAccount.active_sessions ?? oldAccount.active_sessions
})

const syncPaginationAfterLocalRemoval = () => {
  const nextTotal = Math.max(0, pagination.total - 1)
  pagination.total = nextTotal
  pagination.pages = nextTotal > 0 ? Math.ceil(nextTotal / pagination.page_size) : 0

  const maxPage = Math.max(1, pagination.pages || 1)

  if (pagination.page > maxPage) {
    pagination.page = maxPage
  }
  // 行被本地移除后不立刻全量补页，改为提示用户手动同步。
  hasPendingListSync.value = nextTotal > 0
}

const patchAccountInList = (updatedAccount: Account) => {
  const index = accounts.value.findIndex(account => account.id === updatedAccount.id)
  if (index === -1) return
  const mergedAccount = mergeRuntimeFields(accounts.value[index], updatedAccount)
  if (!accountMatchesCurrentFilters(mergedAccount)) {
    accounts.value = accounts.value.filter(account => account.id !== mergedAccount.id)
    syncPaginationAfterLocalRemoval()
    removeSelectedAccounts([mergedAccount.id])
    if (menu.acc?.id === mergedAccount.id) {
      menu.show = false
      menu.acc = null
    }
    return
  }
  const nextAccounts = [...accounts.value]
  nextAccounts[index] = mergedAccount
  accounts.value = nextAccounts
  syncAccountRefs(mergedAccount)
}
const patchUpstreamBillingSnapshot = (accountID: number, snapshot: UpstreamBillingProbeSnapshot) => {
  const account = accounts.value.find(item => item.id === accountID)
  if (!account) return
  upstreamBillingNow.value = Date.now()
  patchAccountInList({
    ...account,
    ...(typeof snapshot.synced_rate_multiplier === 'number'
      ? { rate_multiplier: snapshot.synced_rate_multiplier }
      : {}),
    extra: { ...account.extra, upstream_billing_probe: snapshot }
  })
}
const refreshAccountsAfterUpstreamBillingProbe = async () => {
  await refreshUpstreamBillingSortedList(true)
}
const handleProbeUpstreamBilling = async (account: Account) => {
  if (probingUpstreamBilling.has(account.id)) return
  probingUpstreamBilling.add(account.id)
  try {
    const result = await adminAPI.accounts.probeUpstreamBilling(account.id)
    if (result.snapshot) {
      patchUpstreamBillingSnapshot(account.id, result.snapshot)
      await refreshAccountsAfterUpstreamBillingProbe()
    }
  } catch (error) {
    console.error('Failed to probe upstream billing:', error)
    appStore.showError(extractApiErrorMessage(error, t('admin.accounts.upstreamBilling.probeFailed')))
  } finally {
    probingUpstreamBilling.delete(account.id)
  }
}
const handleAccountUpdated = (updatedAccount: Account) => {
  patchAccountInList(updatedAccount)
  enterAutoRefreshSilentWindow()
}
const formatExportTimestamp = () => {
  const now = new Date()
  const pad2 = (value: number) => String(value).padStart(2, '0')
  return `${now.getFullYear()}${pad2(now.getMonth() + 1)}${pad2(now.getDate())}${pad2(now.getHours())}${pad2(now.getMinutes())}${pad2(now.getSeconds())}`
}
const openExportDataDialog = () => {
  includeProxyOnExport.value = true
  showExportDataDialog.value = true
}
const handleExportData = async () => {
  if (exportingData.value) return
  exportingData.value = true
  try {
    const dataPayload = await accountExportStepUp.run(() => adminAPI.accounts.exportData(
      selIds.value.length > 0
        ? { ids: selIds.value, includeProxies: includeProxyOnExport.value }
        : {
            includeProxies: includeProxyOnExport.value,
            filters: buildAccountQueryFilters()
          }
    ))
    const timestamp = formatExportTimestamp()
    const filename = `sub2api-account-${timestamp}.json`
    const blob = new Blob([JSON.stringify(dataPayload, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = filename
    link.click()
    URL.revokeObjectURL(url)
    // spark 影子账号被后端排除出备份(其凭据透传母账号、调度配置不可经凭据型导入重建);
    // 跳过非零时明确提示用户,避免「下载成功但少了账号」的静默丢失。
    if (dataPayload.skipped_shadows && dataPayload.skipped_shadows > 0) {
      appStore.showWarning(t('admin.accounts.dataExportedSkippedShadows', { count: dataPayload.skipped_shadows }))
    } else {
      appStore.showSuccess(t('admin.accounts.dataExported'))
    }
  } catch (error: any) {
    if (isStepUpCancelled(error)) {
      // 用户主动取消 step-up 验证，静默返回，不弹错误提示。
    } else if (isStepUpBlocked(error)) {
      appStore.showError(
        stepUpBlockReason(error) === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'
          ? t('stepUp.adminApiKeyForbidden')
          : t('stepUp.notEnabled')
      )
    } else {
      appStore.showError(error?.message || t('admin.accounts.dataExportFailed'))
    }
  } finally {
    exportingData.value = false
    showExportDataDialog.value = false
  }
}
const accountExportStepUp = useStepUp()
const closeTestModal = () => { showTest.value = false; testingAcc.value = null }
const closeStatsModal = () => { showStats.value = false; statsAcc.value = null }
const closeReAuthModal = () => { showReAuth.value = false; reAuthAcc.value = null }
const handleTest = (a: Account) => { testingAcc.value = a; showTest.value = true }
const handleViewStats = (a: Account) => { statsAcc.value = a; showStats.value = true }
const handleSchedule = async (a: Account) => {
  scheduleAcc.value = a
  scheduleModelOptions.value = []
  showSchedulePanel.value = true
  try {
    const models = await adminAPI.accounts.getAvailableModels(a.id)
    scheduleModelOptions.value = models.map((m: ClaudeModel) => ({ value: m.id, label: m.display_name || m.id }))
  } catch {
    scheduleModelOptions.value = []
  }
}
const closeSchedulePanel = () => { showSchedulePanel.value = false; scheduleAcc.value = null; scheduleModelOptions.value = [] }
const handleReAuth = (a: Account) => { reAuthAcc.value = a; showReAuth.value = true }
const duplicatingAccountIDs = new Set<number>()
const handleDuplicateAccount = async (a: Account) => {
  if (duplicatingAccountIDs.has(a.id)) return
  duplicatingAccountIDs.add(a.id)
  try {
    const duplicate = await adminAPI.accounts.duplicate(a.id)
    appStore.showSuccess(t('admin.accounts.duplicateSuccess', { name: duplicate.name }))
    reload()
  } catch (error: any) {
    console.error('Failed to duplicate account:', error)
    appStore.showError(error?.message || t('admin.accounts.duplicateFailed'))
  } finally {
    duplicatingAccountIDs.delete(a.id)
  }
}
const handleRefresh = async (a: Account) => {
  try {
    const updated = await adminAPI.accounts.refreshCredentials(a.id)
    patchAccountInList(updated)
    enterAutoRefreshSilentWindow()
  } catch (error) {
    console.error('Failed to refresh credentials:', error)
  }
}
const handleRecoverState = async (a: Account) => {
  try {
    const updated = await adminAPI.accounts.recoverState(a.id)
    patchAccountInList(updated)
    enterAutoRefreshSilentWindow()
    appStore.showSuccess(t('admin.accounts.recoverStateSuccess'))
  } catch (error: any) {
    console.error('Failed to recover account state:', error)
    appStore.showError(error?.message || t('admin.accounts.recoverStateFailed'))
  }
}
const handleResetQuota = async (a: Account) => {
  try {
    const updated = await adminAPI.accounts.resetAccountQuota(a.id)
    patchAccountInList(updated)
    enterAutoRefreshSilentWindow()
    appStore.showSuccess(t('common.success'))
  } catch (error) {
    console.error('Failed to reset quota:', error)
  }
}

const privacyResultMessageKey = (account: Account): { type: 'success' | 'error'; key: string } => {
  const mode = typeof account.extra?.privacy_mode === 'string' ? account.extra.privacy_mode : ''
  if (account.platform === 'openai') {
    switch (mode) {
      case 'training_off':
        return { type: 'success', key: 'admin.accounts.privacyTrainingOff' }
      case 'training_set_cf_blocked':
        return { type: 'error', key: 'admin.accounts.privacyCfBlocked' }
      default:
        return { type: 'error', key: 'admin.accounts.privacyFailed' }
    }
  }
  if (account.platform === 'antigravity') {
    if (mode === 'privacy_set') {
      return { type: 'success', key: 'admin.accounts.privacyAntigravitySet' }
    }
    return { type: 'error', key: 'admin.accounts.privacyAntigravityFailed' }
  }
  return { type: 'error', key: 'admin.accounts.privacyFailed' }
}

const handleSetPrivacy = async (a: Account) => {
  try {
    const updated = await adminAPI.accounts.setPrivacy(a.id)
    patchAccountInList(updated)
    enterAutoRefreshSilentWindow()
    const result = privacyResultMessageKey(updated)
    if (result.type === 'success') {
      appStore.showSuccess(t(result.key))
    } else {
      appStore.showError(t(result.key))
    }
  } catch (error: any) {
    console.error('Failed to set privacy:', error)
    appStore.showError(error?.response?.data?.message || t('admin.accounts.privacyFailed'))
  }
}
const onRevertFallback = async (a: Account) => {
  try {
    await adminAPI.accounts.revertProxyFallback(a.id)
    appStore.showSuccess(t('admin.accounts.revertProxySuccess'))
    reload()
  } catch (error: any) {
    console.error('Failed to revert proxy fallback:', error)
    appStore.showError(error?.response?.data?.message || t('admin.accounts.revertProxyFailed'))
  }
}
const handleCreateSparkShadow = (a: Account) => {
  creatingShadowAcc.value = a
  showCreateShadowDialog.value = true
}
const confirmCreateSparkShadow = async () => {
  const a = creatingShadowAcc.value
  if (!a) return
  try {
    await adminAPI.accounts.createSparkShadow(a.id, { name: `${a.name} (Spark)` })
    showCreateShadowDialog.value = false
    creatingShadowAcc.value = null
    appStore.showSuccess(t('admin.accounts.createSparkShadowSuccess'))
    reload()
  } catch (error: any) {
    console.error('Failed to create spark shadow:', error)
    appStore.showError(error?.response?.data?.message || t('admin.accounts.createSparkShadowFailed'))
  }
}
const handleDelete = (a: Account) => { deletingAcc.value = a; showDeleteDialog.value = true }
const confirmDelete = async () => { if(!deletingAcc.value) return; try { await adminAPI.accounts.delete(deletingAcc.value.id); showDeleteDialog.value = false; deletingAcc.value = null; reload() } catch (error) { console.error('Failed to delete account:', error) } }
const handleToggleSchedulable = async (a: Account) => {
  const nextSchedulable = !a.schedulable
  togglingSchedulable.value = a.id
  try {
    const updated = await adminAPI.accounts.setSchedulable(a.id, nextSchedulable)
    updateSchedulableInList([a.id], updated?.schedulable ?? nextSchedulable)
    enterAutoRefreshSilentWindow()
  } catch (error) {
    console.error('Failed to toggle schedulable:', error)
    appStore.showError(t('admin.accounts.failedToToggleSchedulable'))
  } finally {
    togglingSchedulable.value = null
  }
}
const handleShowTempUnsched = (a: Account) => { tempUnschedAcc.value = a; showTempUnsched.value = true }
const handleTempUnschedReset = async (updated: Account) => {
  showTempUnsched.value = false
  tempUnschedAcc.value = null
  patchAccountInList(updated)
  enterAutoRefreshSilentWindow()
}
const formatExpiresAt = (value: number | null) => {
  if (!value) return '-'
  return formatDateTime(
    new Date(value * 1000),
    {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false
    },
    'sv-SE'
  )
}
const isExpired = (value: number | null) => {
  if (!value) return false
  return value * 1000 <= Date.now()
}
// 所绑定代理的有效期(逻辑同 /admin/proxies,见 utils/proxyExpiry)
const proxyExpiryBadge = (p: AccountProxy): string => proxyExpiryBadgeClass(p.expires_at, p.status)
const proxyExpiryText = (p: AccountProxy): string => {
  const { key, params } = proxyExpiryLabelKey(p.expires_at, p.status)
  return params ? t(key, params) : t(key)
}

// 表格滚动时关闭行操作菜单，并让顶部工具菜单继续贴紧触发按钮。
const handleScroll = () => {
  menu.show = false
  if (showAccountToolsDropdown.value) updateAccountToolsDropdownPosition()
}

const handleViewportResize = () => {
  if (showAccountToolsDropdown.value) updateAccountToolsDropdownPosition()
}

// 点击外部关闭顶部下拉菜单
const handleClickOutside = (event: MouseEvent) => {
  const target = event.target as HTMLElement
  if (accountToolsDropdownRef.value && !accountToolsDropdownRef.value.contains(target)) {
    showAccountToolsDropdown.value = false
  }
  if (autoRefreshDropdownRef.value && !autoRefreshDropdownRef.value.contains(target)) {
    showAutoRefreshDropdown.value = false
  }
}

onMounted(async () => {
  if (typeof window !== 'undefined') {
    desktopViewportMediaQuery = window.matchMedia(desktopViewportQuery)
    isDesktopViewport.value = desktopViewportMediaQuery.matches
    desktopViewportListener = (event: MediaQueryListEvent) => {
      isDesktopViewport.value = event.matches
    }
    if (typeof desktopViewportMediaQuery.addEventListener === 'function') {
      desktopViewportMediaQuery.addEventListener('change', desktopViewportListener)
    } else {
      desktopViewportMediaQuery.addListener(desktopViewportListener)
    }
  }

  load()
  loadUpstreamBillingProbeGlobalState()
  const [proxiesResult, groupsResult] = await Promise.allSettled([
    adminAPI.proxies.getAll(),
    adminAPI.groups.getAll()
  ])
  if (proxiesResult.status === 'fulfilled') {
    proxies.value = proxiesResult.value
  } else {
    console.error('Failed to load proxies:', proxiesResult.reason)
  }
  if (groupsResult.status === 'fulfilled') {
    groups.value = groupsResult.value
  } else {
    console.error('Failed to load groups:', groupsResult.reason)
  }
  window.addEventListener('scroll', handleScroll, true)
  window.addEventListener('resize', handleViewportResize)
  document.addEventListener('click', handleClickOutside)

  if (autoRefreshEnabled.value) {
    autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
    resumeAutoRefresh()
  } else {
    pauseAutoRefresh()
  }
})

onUnmounted(() => {
  if (rescueClockTimer !== null) {
    clearInterval(rescueClockTimer)
    rescueClockTimer = null
  }
  reenableLifecycle.dispose()
  upstreamBillingRateAbortController?.abort()
  invalidateBatchedUsageRequests()
  window.removeEventListener('scroll', handleScroll, true)
  window.removeEventListener('resize', handleViewportResize)
  document.removeEventListener('click', handleClickOutside)
  if (desktopViewportMediaQuery && desktopViewportListener) {
    if (typeof desktopViewportMediaQuery.removeEventListener === 'function') {
      desktopViewportMediaQuery.removeEventListener('change', desktopViewportListener)
    } else {
      desktopViewportMediaQuery.removeListener(desktopViewportListener)
    }
  }
  desktopViewportListener = null
  desktopViewportMediaQuery = null
})
</script>

<style scoped>
.account-tools-menu-item {
  @apply flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm text-gray-700 transition-colors hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-dark-700;
}

.account-tools-menu-icon {
  @apply inline-flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-md;
}
</style>
