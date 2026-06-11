-- ThingModel 新物模型菜单安装脚本
-- 执行此脚本后，租户管理员可在导航中看到新物模型功能入口。
-- 回退：执行 thingmodel_menu_uninstall.sql
-- 默认不执行此脚本，作为可选开启步骤（feature flag）。
-- VITE_AUTH_ROUTE_MODE=dynamic 模式下，菜单不写入此表则 UI 零变化。

-- 父菜单：物模型(Beta)
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000001',
    '0',
    'thingmodel-lab',
    1,
    116,
    '/thingmodel-lab',
    'carbon:model-alt',
    'self',
    '["TENANT_ADMIN","SYS_ADMIN"]'::json,
    '物模型(Beta)',
    NOW(),
    '新物模型体系入口，默认关闭，执行此 SQL 开启',
    'route.thingmodel-lab',
    'layout.base'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：物模型定义
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000002',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'thing-model',
    3,
    1161,
    '/thing-model',
    'mdi:toy-brick-outline',
    'self',
    '["TENANT_ADMIN"]'::json,
    '物模型',
    NOW(),
    '',
    'route.thing-model',
    'layout.base$view.thing-model'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：物模型详情（隐藏，detail 路由）
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000006',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'thing-model_detail',
    3,
    1162,
    '/thing-model/detail',
    '',
    '1',
    '["TENANT_ADMIN"]'::json,
    '物模型详情',
    NOW(),
    '',
    'route.thing-model_detail',
    'view.thing-model_detail'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：设备模板
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000003',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'device-template',
    3,
    1163,
    '/device-template',
    'mdi:file-document-outline',
    'self',
    '["TENANT_ADMIN"]'::json,
    '设备模板',
    NOW(),
    '',
    'route.device-template',
    'layout.base$view.device-template'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：设备模板详情（隐藏）
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000007',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'device-template_detail',
    3,
    1164,
    '/device-template/detail',
    '',
    '1',
    '["TENANT_ADMIN"]'::json,
    '设备模板详情',
    NOW(),
    '',
    'route.device-template_detail',
    'view.device-template_detail'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：产品
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000004',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'tm-product',
    3,
    1165,
    '/tm-product',
    'mdi:package-variant-closed',
    'self',
    '["TENANT_ADMIN"]'::json,
    '产品',
    NOW(),
    '',
    'route.tm-product',
    'layout.base$view.tm-product'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：设备实例
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000005',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'tm-device',
    3,
    1166,
    '/tm-device',
    'mdi:chip',
    'self',
    '["TENANT_ADMIN"]'::json,
    '设备实例',
    NOW(),
    '',
    'route.tm-device',
    'layout.base$view.tm-device'
) ON CONFLICT (id) DO NOTHING;

-- 子菜单：设备实例详情（隐藏）
INSERT INTO public.sys_ui_elements (
    id, parent_id, element_code, element_type, orders,
    param1, param2, param3,
    authority, description, created_at, remark, multilingual, route_path
) VALUES (
    'a1b2c3d4-0000-0000-0000-000000000008',
    'a1b2c3d4-0000-0000-0000-000000000001',
    'tm-device_detail',
    3,
    1167,
    '/tm-device/detail',
    '',
    '1',
    '["TENANT_ADMIN"]'::json,
    '设备实例详情',
    NOW(),
    '',
    'route.tm-device_detail',
    'view.tm-device_detail'
) ON CONFLICT (id) DO NOTHING;
