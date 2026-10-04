import { createHash } from 'node:crypto';

export async function readNativeResponse(response, route) {
  let text;
  try {
    text = await response.text();
  } catch (cause) {
    throw Object.assign(new Error(`Native API ${route}: response body unavailable`, { cause }),
      { httpStatus: response.status, outcomeUnknown: true });
  }
  let body;
  try { body = JSON.parse(text); } catch { /* Keep the HTTP error, even for non-JSON replies. */ }
  if (!response.ok || body === null || typeof body !== 'object' || Array.isArray(body)) {
    const error = new Error(`Native API ${route}: HTTP ${response.status}${response.ok ? ' invalid JSON' : ''}`);
    error.httpStatus = response.status;
    // A gateway timeout or server failure does not prove a mutation stopped.
    error.outcomeUnknown = response.ok || response.status === 408 || response.status >= 500;
    error.responseSha256 = createHash('sha256').update(text).digest('hex');
    error.code = body?.code || body?.error?.code || 'request_failed';
    if (route === '/admin/plugins/upload') {
      const message = body?.message || body?.error?.message || '';
      error.installation_message = String(message)
        .replace(/Bearer\s+\S+/gi, 'Bearer [REDACTED]')
        .replace(/((?:password|token|secret|cookie|authorization|api[_-]?key)\s*[:=]\s*)[^\s,;}]+/gi, '$1[REDACTED]')
        .slice(0, 1500);
    }
    throw error;
  }
  return Object.hasOwn(body, 'data') ? body.data : body;
}

// Native installation requires a disabled plugin. Validate both artifacts
// before stopping it, and reconcile an error before choosing a recovery write.
export async function replacePlugin({
  inspect, disable, upload, enable, preflight, candidate, previous, fence, record = () => {}
}) {
  if (!fence || typeof fence.generation !== 'string' || !fence.generation.trim() ||
      typeof fence.holder !== 'string' || !fence.holder.trim() ||
      typeof fence.assertCurrent !== 'function') {
    throw new Error('An active deployment generation and holder fence is required');
  }
  const binding = Object.freeze({ generation: fence.generation, holder: fence.holder });
  const assertCurrent = fence.assertCurrent.bind(fence);
  const checkFence = async () => {
    try {
      const current = await assertCurrent(binding);
      if (current?.generation !== binding.generation || current?.holder !== binding.holder) {
        throw new Error('Deployment generation or holder changed');
      }
    } catch (cause) {
      throw Object.assign(new Error('Deployment ownership could not be verified', { cause }),
        { code: 'DEPLOYMENT_FENCE_LOST' });
    }
  };
  // The caller must hold its native mutex until all in-flight work terminates.
  // Checks bracket every await so a revoked writer cannot begin another step.
  const step = async (action, ...args) => {
    await checkFence();
    const result = await action(...args, binding);
    await checkFence();
    return result;
  };
  const recordFailure = async (error, failure) => {
    try {
      await record({ ...failure, ...binding });
    } catch (recordError) {
      error.recordError = recordError;
    }
  };
  const identity = (state, artifact) =>
    state.id === previous.id && state.version === artifact.version &&
    state.artifact_sha256 === artifact.sha256 &&
    state.config_sha256 === previous.config_sha256;
  const disabled = state => state.state === 'disabled' &&
    state.bindings.length > 0 && state.bindings.every(binding => !binding.enabled);
  const healthy = state => state.state === 'enabled' && state.runtime_healthy &&
    state.bindings.length > 0 &&
    state.bindings.every(binding => binding.enabled && binding.rollout_percent === 100);
  const requireState = (state, artifact, predicate, message) => {
    if (!identity(state, artifact) || !predicate(state)) throw new Error(message);
  };

  // A rejected preflight cannot enter recovery or touch the running plugin.
  await step(preflight, candidate, previous);
  requireState(await step(inspect), previous, healthy, 'Plugin baseline changed');
  let stage = 'disable';
  try {
    await step(disable, previous.id);
    requireState(await step(inspect), previous, disabled, 'Plugin did not disable cleanly');
    stage = 'upload';
    const installed = await step(upload, candidate);
    if (installed.id !== previous.id || installed.version !== candidate.version) {
      throw new Error('Installed plugin identity/version mismatch');
    }
    requireState(await step(inspect), candidate, disabled, 'Candidate installation readback mismatch');
    stage = 'enable';
    await step(enable, previous.id);
    const result = await step(inspect);
    requireState(result, candidate, healthy, 'Candidate runtime acceptance failed');
    return result;
  } catch (error) {
    const failure = {
      stage, error: error.message, code: error.code || '',
      http_status: error.httpStatus || null,
      response_sha256: error.responseSha256 || '',
      installation_message: error.installation_message || ''
    };
    // A timed-out request can still be executing server-side. Do not race it
    // with a second upload or enable request, even if a read sees the old row.
    if (error.outcomeUnknown) {
      error.recovery = 'reconcile_after_native_operation_quiescence';
      await recordFailure(error, { ...failure, recovery: error.recovery });
      throw error;
    }
    if (error.code === 'DEPLOYMENT_FENCE_LOST') {
      error.recovery = 'reconcile_after_fence_loss';
      await recordFailure(error, { ...failure, recovery: error.recovery });
      throw error;
    }
    try {
      let current = await step(inspect);
      if (identity(current, previous)) {
        if (!healthy(current)) {
          if (['enabled', 'error', 'incompatible'].includes(current.state)) {
            await step(disable, previous.id);
            current = await step(inspect);
          }
          requireState(current, previous, disabled, 'Previous plugin is not safely resumable');
          await step(enable, previous.id);
          current = await step(inspect);
          requireState(current, previous, healthy, 'Previous plugin did not recover');
        }
        error.recovery = 'previous_reenabled_without_upload';
      } else if (identity(current, candidate)) {
        if (['enabled', 'error', 'incompatible'].includes(current.state)) {
          await step(disable, previous.id);
          current = await step(inspect);
        }
        requireState(current, candidate, disabled, 'Candidate is not safely replaceable');
        await step(upload, previous);
        requireState(await step(inspect), previous, disabled, 'Restored package readback mismatch');
        await step(enable, previous.id);
        requireState(await step(inspect), previous, healthy, 'Restored runtime acceptance failed');
        error.recovery = 'previous_package_restored';
      } else {
        throw new Error('Plugin identity/configuration changed; recovery writes refused');
      }
    } catch (recoveryError) {
      error.recovery = recoveryError.code === 'DEPLOYMENT_FENCE_LOST'
        ? 'reconcile_after_fence_loss' : 'recovery_failed';
      error.recoveryError = recoveryError;
      failure.recovery_error = recoveryError.message;
    }
    await recordFailure(error, { ...failure, recovery: error.recovery });
    // Retain the original failure even when recovery itself fails.
    throw error;
  }
}
