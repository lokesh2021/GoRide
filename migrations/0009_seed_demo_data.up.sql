-- Demo data for local dev / manual testing. Fixed, readable api_tokens so
-- curl/Postman examples in README stay stable across DB resets.
INSERT INTO riders (id, name, phone, api_token) VALUES
    ('00000000-0000-0000-0000-000000000001', 'Ananya Rao',   '+919900000001', 'rider1-token'),
    ('00000000-0000-0000-0000-000000000002', 'Karthik Iyer', '+919900000002', 'rider2-token');

INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token) VALUES
    ('00000000-0000-0000-0000-000000000011', 'Suresh Kumar',    '+919900000011', 'BLR', 'mini',  'Maruti Alto',    'KA-01-AB-1234', 4.5, 'available', 'driver1-token'),
    ('00000000-0000-0000-0000-000000000012', 'Manjunath Gowda', '+919900000012', 'BLR', 'mini',  'Hyundai Santro', 'KA-01-AB-2345', 4.6, 'available', 'driver2-token'),
    ('00000000-0000-0000-0000-000000000013', 'Ramesh Naik',     '+919900000013', 'BLR', 'sedan', 'Honda City',     'KA-01-CD-3456', 4.7, 'available', 'driver3-token'),
    ('00000000-0000-0000-0000-000000000014', 'Prakash Shetty',  '+919900000014', 'BLR', 'sedan', 'Toyota Etios',   'KA-01-CD-4567', 4.8, 'available', 'driver4-token'),
    ('00000000-0000-0000-0000-000000000015', 'Girish Reddy',    '+919900000015', 'BLR', 'xl',    'Toyota Innova',  'KA-01-EF-5678', 4.9, 'available', 'driver5-token'),
    ('00000000-0000-0000-0000-000000000016', 'Vinod Achar',     '+919900000016', 'BLR', 'xl',    'Maruti Ertiga',  'KA-01-EF-6789', 4.5, 'available', 'driver6-token');
