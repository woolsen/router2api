/**
 * 按角色解析登录后的落点。
 * admin → 管理后台，merchant → 商户后台，其余 → 用户面板。
 */
export function resolveLandingPath(role: string | null | undefined): string {
  if (role === 'admin') return '/admin/dashboard'
  if (role === 'merchant') return '/merchant/dashboard'
  return '/dashboard'
}
